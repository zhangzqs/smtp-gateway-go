package logger

import (
	"errors"
	"os"

	"github.com/rs/zerolog"
)

type Config struct {
	Level string `yaml:"level"`
}

func (c Config) Validate() error {
	switch c.Level {
	case "trace", "debug", "info", "warn", "error":
		return nil
	default:
		return errors.New("logger.level must be trace, debug, info, warn, or error")
	}
}

func New(cfg Config) (zerolog.Logger, error) {
	if err := cfg.Validate(); err != nil {
		return zerolog.Logger{}, err
	}
	level, err := zerolog.ParseLevel(cfg.Level)
	if err != nil {
		return zerolog.Logger{}, err
	}
	return zerolog.New(os.Stderr).Level(level).With().Timestamp().Logger(), nil
}
