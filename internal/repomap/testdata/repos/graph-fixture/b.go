package graphfixture

// Widget is the central type of this fixture: it is referenced by a.go and c.go
// but b.go references nothing they define, so PageRank centrality(b) is highest.
type Widget struct {
	Name string
}

// NewWidget constructs a Widget.
func NewWidget(name string) *Widget {
	return &Widget{Name: name}
}
