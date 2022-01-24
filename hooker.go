package shell

type Hooker interface {
	Before(Command) Command
}
