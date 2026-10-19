package database

import (
	"context"
	"database/sql"
	"errors"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
)

type Config struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

func (c Config) Validate() error {
	if c.Driver != "sqlite" && c.Driver != "postgres" {
		return errors.New("database.driver must be sqlite or postgres")
	}
	if c.DSN == "" {
		return errors.New("database.dsn is required")
	}
	return nil
}

type Database struct {
	db     *gorm.DB
	sql    *sql.DB
	driver string
}

func New(ctx context.Context, cfg Config) (*Database, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var sqlDB *sql.DB
	var dialector gorm.Dialector
	var err error
	if cfg.Driver == "sqlite" {
		sqlDB, err = sql.Open("sqlite", cfg.DSN)
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
			if _, err := sqlDB.ExecContext(ctx, pragma); err != nil {
				_ = sqlDB.Close()
				return nil, err
			}
		}
		dialector = sqlite.New(sqlite.Config{Conn: sqlDB})
	} else {
		dialector = postgres.Open(cfg.DSN)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		return nil, errors.New("database connection failed")
	}
	if sqlDB == nil {
		sqlDB, err = db.DB()
		if err != nil {
			return nil, err
		}
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, errors.New("database connection failed")
	}
	return &Database{db: db, sql: sqlDB, driver: cfg.Driver}, nil
}

func (d *Database) DB() *gorm.DB   { return d.db }
func (d *Database) SQL() *sql.DB   { return d.sql }
func (d *Database) Driver() string { return d.driver }
func (d *Database) Close() error   { return d.sql.Close() }
