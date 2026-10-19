package logger

import (
	"fmt"
	"os"

	"github.com/rs/zerolog"
)

type Config struct {
	Level string `yaml:"level"`
}

func (c Config) Validate() error {
	if _, err := zerolog.ParseLevel(c.Level); err != nil {
		return fmt.Errorf("logger.level: %w", err)
	}
	return nil
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
