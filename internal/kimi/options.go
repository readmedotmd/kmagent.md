package kimi

// Options configures the Kimi transport
type Options struct {
	WorkDir   string
	Model     string
	Thinking  bool
	YoloMode  bool
	SessionID string
	Env       map[string]string
}

// Option is a functional option for configuring the transport
type Option func(*Options)

// WithWorkDir sets the working directory
func WithWorkDir(dir string) Option {
	return func(o *Options) {
		o.WorkDir = dir
	}
}

// WithModel sets the model
func WithModel(model string) Option {
	return func(o *Options) {
		o.Model = model
	}
}

// WithThinking enables thinking mode
func WithThinking(enabled bool) Option {
	return func(o *Options) {
		o.Thinking = enabled
	}
}

// WithYoloMode enables auto-approve mode
func WithYoloMode(enabled bool) Option {
	return func(o *Options) {
		o.YoloMode = enabled
	}
}

// WithSessionID sets the session ID to resume
func WithSessionID(sessionID string) Option {
	return func(o *Options) {
		o.SessionID = sessionID
	}
}

// WithEnv sets additional environment variables
func WithEnv(env map[string]string) Option {
	return func(o *Options) {
		o.Env = env
	}
}

// NewOptions creates Options from functional options
func NewOptions(opts ...Option) *Options {
	options := &Options{}
	for _, opt := range opts {
		opt(options)
	}
	return options
}
