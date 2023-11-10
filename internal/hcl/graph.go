package hcl

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/heimdalr/dag"
	"github.com/rs/zerolog"
	"github.com/zclconf/go-cty/cty"
)

var (
	addrSplitModuleRegex = regexp.MustCompile(`^((?:module\.[^.]+\.?)+)\.(.*)$`)
)

type ModuleConfig struct {
	name            string
	moduleCall      *ModuleCall
	evaluator       *Evaluator
	parentEvaluator *Evaluator
}

type ModuleConfigs struct {
	configs map[string][]ModuleConfig
	mu      sync.RWMutex
}

func NewModuleConfigs() *ModuleConfigs {
	return &ModuleConfigs{
		configs: make(map[string][]ModuleConfig),
		mu:      sync.RWMutex{},
	}
}

func (m *ModuleConfigs) Add(moduleAddress string, moduleConfig ModuleConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.configs[moduleAddress]; !ok {
		m.configs[moduleAddress] = []ModuleConfig{}
	}

	m.configs[moduleAddress] = append(m.configs[moduleAddress], moduleConfig)
}

func (m *ModuleConfigs) Get(moduleAddress string) []ModuleConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.configs[moduleAddress]
}

type Graph struct {
	dag           *dag.DAG
	logger        zerolog.Logger
	rootVertex    Vertex
	vertexMutex   *sync.Mutex
	moduleConfigs *ModuleConfigs
}

type Vertex interface {
	ID() string
	ModuleAddress() string
	Visit(mutex *sync.Mutex) error
	References() []VertexReference
}

type VertexReference struct {
	ModuleAddress string
	Key           string
}

func NewGraphWithRoot(logger zerolog.Logger, vertexMutex *sync.Mutex) (*Graph, error) {
	if vertexMutex == nil {
		vertexMutex = &sync.Mutex{}
	}
	g := &Graph{
		dag:           dag.NewDAG(),
		logger:        logger,
		moduleConfigs: NewModuleConfigs(),
		vertexMutex:   vertexMutex,
	}

	g.rootVertex = &VertexRoot{}

	err := g.dag.AddVertexByID(g.rootVertex.ID(), g.rootVertex)
	if err != nil {
		return nil, fmt.Errorf("error adding vertex %q %w", g.rootVertex.ID(), err)
	}

	return g, nil
}

func (g *Graph) ReduceTransitively() {
	g.dag.ReduceTransitively()
}

func (g *Graph) Populate(evaluator *Evaluator) error {
	var vertexes []Vertex

	blocks, err := g.loadAllBlocks(evaluator)
	if err != nil {
		return err
	}

	for _, block := range blocks {
		switch block.Type() {
		case "locals":
			for _, attr := range block.GetAttributes() {
				vertexes = append(vertexes, &VertexLocal{
					logger:        g.logger,
					moduleConfigs: g.moduleConfigs,
					block:         block,
					attr:          attr,
				})
			}
		case "module":
			vertexes = append(vertexes, &VertexModuleCall{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
			vertexes = append(vertexes, &VertexModuleExit{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		case "variable":
			vertexes = append(vertexes, &VertexVariable{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		case "output":
			vertexes = append(vertexes, &VertexOutput{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		case "provider":
			vertexes = append(vertexes, &VertexProvider{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		case "resource":
			vertexes = append(vertexes, &VertexResource{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		case "data":
			vertexes = append(vertexes, &VertexData{
				logger:        g.logger,
				moduleConfigs: g.moduleConfigs,
				block:         block,
			})
		}
	}

	for _, vertex := range vertexes {
		id := vertex.ID()

		g.logger.Debug().Msgf("adding vertex: %s", id)
		err := g.dag.AddVertexByID(id, vertex)
		if err != nil {
			// We don't actually mind if blocks are added multiple times
			// since this helps us support cases like _override.tf files
			// and in-progress changes.
			g.logger.Debug().Err(err).Msgf("error adding vertex %q", id)
		}
	}

	edges := make([]dag.EdgeInput, 0)

	for _, vertex := range vertexes {
		id := vertex.ID()
		modAddr := vertex.ModuleAddress()

		if modAddr == "" {
			g.logger.Debug().Msgf("adding edge: %s, %s", g.rootVertex.ID(), id)
			edges = append(edges, dag.EdgeInput{
				SrcID: g.rootVertex.ID(),
				DstID: id,
			})
		} else {
			// Add the module call edge
			g.logger.Debug().Msgf("adding edge: %s, %s", moduleCallID(modAddr), id)
			edges = append(edges, dag.EdgeInput{
				SrcID: moduleCallID(modAddr),
				DstID: id,
			})

			// Add the module exit edge
			g.logger.Debug().Msgf("adding edge: %s, %s", id, modAddr)
			edges = append(edges, dag.EdgeInput{
				SrcID: id,
				DstID: modAddr,
			})
		}

		for _, ref := range vertex.References() {
			var srcId string

			parts := strings.Split(ref.Key, ".")
			idx := len(parts)

			// data references should always have a length of 3
			// provider references might have a length of 3 (if using an alias) or 2 (if not).
			if (strings.HasPrefix(ref.Key, "data.") || strings.HasPrefix(ref.Key, "provider.")) && len(parts) >= 3 {
				// Source ID is the first 3 parts or less if the length of parts is less than 3
				idx = 3
			} else if len(parts) >= 2 {
				// variable, local, resources and output references should all have length 2
				idx = 2
			}
			srcId = strings.Join(parts[:idx], ".")

			// Don't add the module prefix for providers since they are
			// evaluated in the root module
			if !strings.HasPrefix(srcId, "provider.") && ref.ModuleAddress != "" {
				modAddress := stripCount(ref.ModuleAddress)

				srcId = fmt.Sprintf("%s.%s", modAddress, srcId)
			}

			// Strip the count/index suffix from the source ID
			srcId = stripCount(srcId)

			if srcId == id {
				continue
			}

			// Check if the source vertex exists
			_, err := g.dag.GetVertex(srcId)
			if err == nil {
				g.logger.Debug().Msgf("adding edge: %s, %s", srcId, id)
				edges = append(edges, dag.EdgeInput{
					SrcID: srcId,
					DstID: id,
				})

				continue
			}

			// If the source vertex doesn't exist, it might be a module output attribute,
			// so we need to check if the module output exists and add an edge from that
			// to the current vertex instead.
			if ref.ModuleAddress != "" {
				modAddress := stripCount(ref.ModuleAddress)

				srcId = fmt.Sprintf("%s.%s", modAddress, parts[0])

				// Check if the source vertex exists
				_, err := g.dag.GetVertex(srcId)
				if err == nil {
					g.logger.Debug().Msgf("adding edge: %s, %s", srcId, id)
					edges = append(edges, dag.EdgeInput{
						SrcID: srcId,
						DstID: id,
					})

					continue
				}
			}
		}
	}

	err = g.dag.AddEdges(edges)
	if err != nil {
		return fmt.Errorf("error adding edges %w", err)
	}

	// Setup initial context
	g.moduleConfigs.Add("", ModuleConfig{
		name:       "",
		moduleCall: nil,
		evaluator:  evaluator,
	})

	evaluator.ctx.Set(cty.ObjectVal(map[string]cty.Value{}), "var")
	evaluator.ctx.Set(cty.ObjectVal(map[string]cty.Value{}), "data")
	evaluator.ctx.Set(cty.ObjectVal(map[string]cty.Value{}), "local")
	evaluator.ctx.Set(cty.ObjectVal(map[string]cty.Value{}), "output")

	// TODO: is there a better way of doing this?
	// Add the locals block to the list of filtered blocks. These won't get
	// added when we walk the graph since locals are evaluated per attribute
	// and not per block, so we need to make sure this is done here.
	evaluator.AddFilteredBlocks(evaluator.module.Blocks.OfType("locals")...)

	return nil
}

func (g *Graph) AsJSON() ([]byte, error) {
	return g.dag.MarshalJSON()
}

func (g *Graph) Walk() {
	v := NewGraphVisitor(g.logger, g.vertexMutex)

	flowCallback := func(d *dag.DAG, id string, parentResults []dag.FlowResult) (interface{}, error) {
		vertex, _ := d.GetVertex(id)

		v.Visit(id, vertex)

		return vertex, nil
	}

	_, _ = g.dag.DescendantsFlow(g.rootVertex.ID(), nil, flowCallback)
}

func (g *Graph) Run(evaluator *Evaluator) (*Module, error) {
	err := g.Populate(evaluator)
	if err != nil {
		return nil, err
	}

	g.ReduceTransitively()
	g.Walk()

	evaluator.module.Blocks = evaluator.filteredBlocks
	evaluator.module = *evaluator.collectModules()

	return &evaluator.module, nil
}

type GraphVisitor struct {
	logger      zerolog.Logger
	vertexMutex *sync.Mutex
}

func NewGraphVisitor(logger zerolog.Logger, vertexMutex *sync.Mutex) *GraphVisitor {
	return &GraphVisitor{
		logger:      logger,
		vertexMutex: vertexMutex,
	}
}

func (v *GraphVisitor) Visit(id string, vertex interface{}) {
	v.logger.Debug().Msgf("visiting vertex %q", id)

	vert := vertex.(Vertex)
	err := vert.Visit(v.vertexMutex)
	if err != nil {
		v.logger.Debug().Err(err).Msgf("ignoring vertex %q because an error was encountered", id)
	}
}

func (g *Graph) loadAllBlocks(evaluator *Evaluator) ([]*Block, error) {
	return g.loadBlocksForModule(evaluator)
}

func (g *Graph) loadBlocksForModule(evaluator *Evaluator) ([]*Block, error) {
	var blocks []*Block

	for _, block := range evaluator.module.Blocks {
		blocks = append(blocks, block)

		if block.Type() == "module" {
			modCall, err := evaluator.loadModule(block)
			if err != nil {
				return nil, fmt.Errorf("could not load module %q", block.FullName())
			}

			moduleEvaluator := NewEvaluator(
				*modCall.Module,
				evaluator.workingDir,
				map[string]cty.Value{},
				evaluator.moduleMetadata,
				map[string]map[string]cty.Value{},
				evaluator.workspace,
				evaluator.blockBuilder,
				nil,
				evaluator.logger,
				&Context{},
			)

			modBlocks, err := g.loadBlocksForModule(moduleEvaluator)
			if err != nil {
				return nil, fmt.Errorf("could not load blocks for module %q", block.FullName())
			}

			blocks = append(blocks, modBlocks...)
		}
	}

	return blocks, nil
}