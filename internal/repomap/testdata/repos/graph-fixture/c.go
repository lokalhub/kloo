package graphfixture

// BuildC also references Widget defined in b.go, creating edge c→b.
func BuildC() *Widget {
	w := NewWidget("c")
	return w
}
