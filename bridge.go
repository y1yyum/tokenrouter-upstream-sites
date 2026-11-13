package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "github.com/lib/pq"
)

// bridge.go calls a restricted database capability for atomic provider ownership.
// The login has no table privileges. Its two functions derive targets from saved
// plugin state, preserve external scheduling intent and publish host outbox events.

var ErrBridge = errors.New("provider control bridge unavailable")

// ControlBridge separates policy evaluation from the atomic scheduling capability.
type ControlBridge interface {
	Reconcile(context.Context) error
	Snapshot(context.Context) (json.RawMessage, error)
}

// SQLBridge owns a small pool authenticated as the execute-only control login.
type SQLBridge struct{ db *sql.DB }

// openBridge refuses control until the installed guard and function version match.
func openBridge(ctx context.Context, dsn string) (*SQLBridge, error) {
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		return nil, ErrBridge
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(15 * time.Minute)
	b := &SQLBridge{db: db}
	if _, e = b.Snapshot(ctx); e != nil {
		db.Close()
		return nil, ErrBridge
	}
	return b, nil
}

// Snapshot verifies the manual-intent trigger before exposing sanitized status.
func (b *SQLBridge) Snapshot(parent context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var raw []byte
	if b.db.QueryRowContext(ctx, "SELECT upstream_site_control.snapshot()").Scan(&raw) != nil {
		return nil, ErrBridge
	}
	var status struct {
		Version int  `json:"version"`
		Ready   bool `json:"trigger_ready"`
	}
	if json.Unmarshal(raw, &status) != nil || status.Version != 1 || !status.Ready {
		return nil, ErrBridge
	}
	return json.RawMessage(raw), nil
}

// Reconcile applies the current persisted policy through one atomic host transaction.
func (b *SQLBridge) Reconcile(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if _, e := b.Snapshot(ctx); e != nil {
		return e
	}
	var raw []byte
	if b.db.QueryRowContext(ctx, "SELECT upstream_site_control.reconcile()").Scan(&raw) != nil {
		return ErrBridge
	}
	return nil
}

// reconcile retries durable desired state after process exits or transient DB failures.
func (p *Plugin) reconcile(ctx context.Context) error {
	if p.bridge == nil {
		return nil
	}
	return p.bridge.Reconcile(ctx)
}

// Shutdown releases transport resources while leaving active holds fail-closed.
// Disabling a plugin must not silently reopen an unhealthy upstream. Administrators
// turn a site's control mode off before uninstalling to release its owned holds.
func (p *Plugin) Shutdown(context.Context) error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	if b, ok := p.bridge.(*SQLBridge); ok {
		return b.db.Close()
	}
	return nil
}
