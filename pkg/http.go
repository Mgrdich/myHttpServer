package pkg

type contextKey struct {
	name string
}

func (k *contextKey) String() string {
	return "net/http context value " + k.name
}
