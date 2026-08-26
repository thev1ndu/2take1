package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

const schema = `
CREATE TABLE IF NOT EXISTS todos (
	id      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
	title   VARCHAR(500)    NOT NULL,
	done    BOOLEAN         NOT NULL DEFAULT FALSE,
	created TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

func open() (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = env("DB_ADDR", "mysql:3306")
	cfg.User = env("DB_USER", "2take1")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.DBName = env("DB_NAME", "2take1")
	cfg.Collation = "utf8mb4_unicode_ci"
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second

	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)

	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

func waitReady(ctx context.Context, db *sql.DB) error {
	const retry = 2 * time.Second
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("database never became ready: %w", err)
		}
		log.Printf("database not ready (attempt %d): %v", attempt, err)

		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return fmt.Errorf("database never became ready: %w", err)
		}
	}
}
