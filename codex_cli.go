package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func runCodexCLI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing codex subcommand")
	}
	switch args[0] {
	case "login":
		return runCodexLoginCommand(args[1:])
	case "list":
		return runCodexListCommand(args[1:])
	case "checkout":
		return runCodexCheckoutCommand(args[1:])
	default:
		return fmt.Errorf("unknown codex subcommand %q", args[0])
	}
}

func runCodexLoginCommand(args []string) error {
	fs := flag.NewFlagSet("codex login", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "config file path")
	providerName := fs.String("provider", "", "codex provider name")
	noBrowser := fs.Bool("no-browser", false, "don't open browser automatically")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	provider, err := selectCodexProvider(cfg, *providerName)
	if err != nil {
		return err
	}
	client, err := newHTTPClient(provider.Proxy, provider.CAFile, provider.Insecure)
	if err != nil {
		return err
	}
	pool, err := newCodexAccountPool(provider.AuthDir, client)
	if err != nil {
		return err
	}
	authURL, verifier, state, err := buildCodexLoginURL()
	if err != nil {
		return err
	}
	fmt.Printf("Starting local login server on %s.\n", codexRedirectURI)
	fmt.Println("If your browser did not open, navigate to this URL to authenticate:")
	fmt.Println()
	fmt.Printf("%s\n\n", authURL)
	if !*noBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Printf("Browser open failed: %v\n", err)
		}
	}
	code, err := waitForCodexOAuthCallback(state, 5*time.Minute)
	if err != nil {
		return err
	}
	account, err := exchangeCodexCode(context.Background(), client, code, verifier)
	if err != nil {
		return err
	}
	if err := pool.save(account); err != nil {
		return err
	}
	fmt.Printf("Codex login successful. saved id=%s email=%s auth_dir=%s\n", account.ID, account.Email, provider.AuthDir)
	return nil
}

func runCodexListCommand(args []string) error {
	fs := flag.NewFlagSet("codex list", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "config file path")
	providerName := fs.String("provider", "", "codex provider name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	provider, err := selectCodexProvider(cfg, *providerName)
	if err != nil {
		return err
	}
	client, err := newHTTPClient(provider.Proxy, provider.CAFile, provider.Insecure)
	if err != nil {
		return err
	}
	pool, err := newCodexAccountPool(provider.AuthDir, client)
	if err != nil {
		return err
	}
	accounts := pool.list()
	if len(accounts) == 0 {
		fmt.Println("No codex accounts found.")
		return nil
	}
	for _, account := range accounts {
		fmt.Printf("%s\t%s\t%s\n", account.ID, account.Email, account.AccountID)
	}
	return nil
}

func runCodexCheckoutCommand(args []string) error {
	fs := flag.NewFlagSet("codex checkout", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "config file path")
	providerName := fs.String("provider", "", "codex provider name")
	accountID := fs.String("id", "", "account id to checkout")
	targetPath := fs.String("target", "", "target auth.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	provider, err := selectCodexProvider(cfg, *providerName)
	if err != nil {
		return err
	}
	client, err := newHTTPClient(provider.Proxy, provider.CAFile, provider.Insecure)
	if err != nil {
		return err
	}
	pool, err := newCodexAccountPool(provider.AuthDir, client)
	if err != nil {
		return err
	}
	account, err := selectCodexAccountForCheckout(pool, strings.TrimSpace(*accountID))
	if err != nil {
		return err
	}
	outPath := strings.TrimSpace(*targetPath)
	if outPath == "" {
		outPath = filepath.Join(mustUserHomeDir(), ".codex", "auth.json")
	}
	if err := mergeCodexAccountToAuthFile(outPath, account); err != nil {
		return err
	}
	fmt.Printf("Checked out codex account %s to %s\n", account.ID, outPath)
	return nil
}

func selectCodexProvider(cfg *Config, providerName string) (*ProviderConfig, error) {
	var matches []*ProviderConfig
	for i := range cfg.Providers {
		prov := &cfg.Providers[i]
		if !strings.EqualFold(strings.TrimSpace(prov.Type), "codex") {
			continue
		}
		if trimmed := strings.TrimSpace(providerName); trimmed != "" {
			if prov.Name == trimmed {
				return prov, nil
			}
			continue
		}
		matches = append(matches, prov)
	}
	if strings.TrimSpace(providerName) != "" {
		return nil, fmt.Errorf("codex provider %q not found", providerName)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no codex provider configured")
	}
	return nil, fmt.Errorf("multiple codex providers configured; pass --provider")
}

func selectCodexAccountForCheckout(pool *codexAccountPool, wantedID string) (*codexAccount, error) {
	accounts := pool.list()
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no codex accounts found")
	}
	if wantedID != "" {
		for _, account := range accounts {
			if account.ID == wantedID {
				return account, nil
			}
		}
		return nil, fmt.Errorf("codex account %q not found", wantedID)
	}
	if len(accounts) == 1 {
		return accounts[0], nil
	}
	var ids []string
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return nil, fmt.Errorf("multiple accounts found; pass --id (%s)", strings.Join(ids, ", "))
}

func waitForCodexOAuthCallback(state string, timeout time.Duration) (string, error) {
	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)
	mux := http.NewServeMux()
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", codexCallbackPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		gotState := strings.TrimSpace(r.URL.Query().Get("state"))
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if errText := strings.TrimSpace(r.URL.Query().Get("error")); errText != "" {
			http.Error(w, errText, http.StatusBadRequest)
			select {
			case resultCh <- result{err: fmt.Errorf("oauth error: %s", errText)}:
			default:
			}
			return
		}
		if gotState != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			select {
			case resultCh <- result{err: fmt.Errorf("invalid state")}:
			default:
			}
			return
		}
		_, _ = w.Write([]byte("<html><body><h1>Codex login successful</h1><p>You can close this window.</p></body></html>"))
		select {
		case resultCh <- result{code: code}:
		default:
		}
	})
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return "", err
	}
	defer ln.Close()
	go func() {
		_ = server.Serve(ln)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-resultCh:
		return res.code, res.err
	case <-timer.C:
		return "", fmt.Errorf("timeout waiting for OAuth callback")
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return errors.New("unsupported platform for automatic browser launch")
	}
	return cmd.Start()
}

func mergeCodexAccountToAuthFile(path string, account *codexAccount) error {
	if account == nil {
		return fmt.Errorf("nil codex account")
	}
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = account.AccessToken
	if account.RefreshToken != "" {
		tokens["refresh_token"] = account.RefreshToken
	}
	if account.IDToken != "" {
		tokens["id_token"] = account.IDToken
	}
	if account.AccountID != "" {
		tokens["account_id"] = account.AccountID
	}
	doc["tokens"] = tokens
	if account.UpdatedAt.IsZero() {
		doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	} else {
		doc["last_refresh"] = account.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func mustUserHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
