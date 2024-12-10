package goinject

type config struct {
	logger Logger
}

type Option func(*config)

type Logger interface {
	Printf(format string, v ...any)
}

type noopLogger struct{}

func (nl noopLogger) Printf(format string, v ...any) {
	// NOOP
}

func WithLogger(logger Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}
