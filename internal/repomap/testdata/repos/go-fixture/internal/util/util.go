package util

type Config struct {
	Name string
}

const DefaultName = "kloo"

func Normalize(s string) string { return s }
