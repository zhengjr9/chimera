package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type codexAccount struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	AccountID    string    `json:"account_id"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type codexAccountPool struct {
	dir    string
	client *http.Client

	mu       sync.Mutex
	accounts map[string]*codexAccount
	order    []string
	cursor   int
}

func newCodexAccountPool(dir string, client *http.Client) (*codexAccountPool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pool := &codexAccountPool{
		dir:      dir,
		client:   client,
		accounts: make(map[string]*codexAccount),
	}
	if err := pool.load(); err != nil {
		return nil, err
	}
	return pool, nil
}

func (p *codexAccountPool) load() error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = make(map[string]*codexAccount)
	p.order = p.order[:0]
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.dir, entry.Name()))
		if err != nil {
			return err
		}
		account, err := parseCodexAccountFile(entry.Name(), data)
		if err != nil {
			return err
		}
		if account.ID == "" {
			account.ID = strings.TrimSuffix(entry.Name(), ".json")
		}
		acc := *account
		p.accounts[acc.ID] = &acc
		p.order = append(p.order, acc.ID)
	}
	sort.Strings(p.order)
	return nil
}

func parseCodexAccountFile(filename string, data []byte) (*codexAccount, error) {
	var account codexAccount
	if err := json.Unmarshal(data, &account); err == nil && strings.TrimSpace(account.AccessToken) != "" {
		if account.ID == "" {
			account.ID = strings.TrimSuffix(filename, ".json")
		}
		return &account, nil
	}

	root := gjson.ParseBytes(data)
	tokens := root.Get("tokens")
	if !tokens.Exists() || !tokens.IsObject() {
		if err := json.Unmarshal(data, &account); err != nil {
			return nil, err
		}
		if account.ID == "" {
			account.ID = strings.TrimSuffix(filename, ".json")
		}
		return &account, nil
	}

	account = codexAccount{
		ID:           strings.TrimSuffix(filename, ".json"),
		AccessToken:  strings.TrimSpace(tokens.Get("access_token").String()),
		RefreshToken: strings.TrimSpace(tokens.Get("refresh_token").String()),
		IDToken:      strings.TrimSpace(tokens.Get("id_token").String()),
		AccountID:    strings.TrimSpace(tokens.Get("account_id").String()),
	}
	if email, accountID := parseCodexIDToken(account.IDToken); email != "" || accountID != "" {
		if email != "" {
			account.Email = email
		}
		if account.AccountID == "" {
			account.AccountID = accountID
		}
	}
	if account.ID == "" {
		account.ID = buildCodexAccountID(account.Email, account.AccountID)
	}
	if ts := strings.TrimSpace(root.Get("last_refresh").String()); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			account.UpdatedAt = parsed.UTC()
		}
	}
	return &account, nil
}

func (p *codexAccountPool) save(account *codexAccount) error {
	if account == nil {
		return fmt.Errorf("nil codex account")
	}
	if account.ID == "" {
		account.ID = buildCodexAccountID(account.Email, account.AccountID)
	}
	if account.CreatedAt.IsZero() {
		account.CreatedAt = time.Now().UTC()
	}
	account.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.dir, account.ID+".json"), data, 0o600); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := *account
	if _, ok := p.accounts[acc.ID]; !ok {
		p.order = append(p.order, acc.ID)
		sort.Strings(p.order)
	}
	p.accounts[acc.ID] = &acc
	return nil
}

func (p *codexAccountPool) list() []*codexAccount {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*codexAccount, 0, len(p.order))
	for _, id := range p.order {
		if acc := p.accounts[id]; acc != nil {
			copied := *acc
			out = append(out, &copied)
		}
	}
	return out
}

func (p *codexAccountPool) pick(ctx context.Context) (*codexAccount, error) {
	if p == nil {
		return nil, fmt.Errorf("codex auth pool not configured")
	}
	p.mu.Lock()
	if len(p.order) == 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("no codex accounts configured")
	}
	ids := append([]string(nil), p.order...)
	start := p.cursor
	p.cursor++
	p.mu.Unlock()
	var lastErr error
	for i := 0; i < len(ids); i++ {
		account := p.get(ids[(start+i)%len(ids)])
		if account == nil {
			continue
		}
		if updated, err := p.ensureFresh(ctx, account); err == nil {
			return updated, nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no usable codex accounts")
}

func (p *codexAccountPool) get(id string) *codexAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := p.accounts[id]
	if acc == nil {
		return nil
	}
	copied := *acc
	return &copied
}

func (p *codexAccountPool) ensureFresh(ctx context.Context, account *codexAccount) (*codexAccount, error) {
	if strings.TrimSpace(account.AccessToken) == "" {
		return nil, fmt.Errorf("codex access token missing for %s", account.Email)
	}
	if account.ExpiresAt.IsZero() || time.Until(account.ExpiresAt) > 24*time.Hour {
		return account, nil
	}
	updated, err := refreshCodexAccount(ctx, p.client, account)
	if err != nil {
		return nil, err
	}
	if err := p.save(updated); err != nil {
		return nil, err
	}
	return updated, nil
}
