package graphfixture

// BuildA references Widget/NewWidget defined in b.go, creating edge a→b.
func BuildA() *Widget {
	return NewWidget("a")
}
