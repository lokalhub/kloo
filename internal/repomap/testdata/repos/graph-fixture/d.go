package graphfixture

// Gadget is defined here but referenced by no other fixture file, and d.go
// references nothing a/b/c define — so d is unreferenced (lowest centrality).
type Gadget struct {
	id int
}

func newGadget() *Gadget {
	return &Gadget{id: 0}
}
