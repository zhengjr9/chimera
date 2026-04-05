package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	defaultKiroCLIAuthDir  = "/tmp/kiro"
	defaultKiroCLIRegion   = "us-east-1"
	defaultKiroCLIStartURL = "https://view.awsapps.com/start"
)

func runKiroLoginCLI(args []string) error {
	fs := flag.NewFlagSet("kiro-login", flag.ContinueOnError)
	authDir := fs.String("auth-dir", defaultKiroCLIAuthDir, "directory to store kiro token file")
	region := fs.String("region", defaultKiroCLIRegion, "AWS region for Builder ID device flow")
	startURL := fs.String("start-url", defaultKiroCLIStartURL, "Builder ID start URL")
	identityID := fs.String("identity-id", "", "existing Cognito IdentityId to use when fetching AWS temporary credentials")
	identityPoolID := fs.String("identity-pool-id", kiroDefaultIdentityPool, "Cognito identity pool id associated with the account")
	noBrowser := fs.Bool("no-browser", false, "do not open browser automatically")
	if err := fs.Parse(args); err != nil {
		return err
	}

	registration, err := registerKiroOIDCClient(context.Background(), *region)
	if err != nil {
		return err
	}
	deviceAuth, err := startKiroDeviceAuthorization(context.Background(), *region, registration.ClientID, registration.ClientSecret, *startURL)
	if err != nil {
		return err
	}
	resolvedIdentityID := firstNonEmpty(strings.TrimSpace(*identityID), loadExistingKiroIdentity(strings.TrimSpace(*authDir)))

	fmt.Printf("Open this URL in your browser:\n%s\n\n", firstNonEmpty(deviceAuth.VerificationURIComplete, deviceAuth.VerificationURI))
	if deviceAuth.UserCode != "" {
		fmt.Printf("Enter this code if prompted:\n%s\n\n", deviceAuth.UserCode)
	}
	fmt.Printf("Token will be saved to %s\n", filepath.Join(strings.TrimSpace(*authDir), kiroAuthTokenFile))
	if resolvedIdentityID == "" {
		fmt.Println("Cognito AWS credentials will be skipped this time because no IdentityId is available.")
	}
	fmt.Println("Waiting for device authorization...")
	if !*noBrowser {
		targetURL := firstNonEmpty(deviceAuth.VerificationURIComplete, deviceAuth.VerificationURI)
		if err := openKiroBrowser(targetURL); err != nil {
			fmt.Printf("Failed to open browser automatically: %v\n", err)
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
		return fmt.Errorf("interrupted")
	default:
	}

	done := make(chan error, 1)
	go func() {
		creds, err := pollKiroDeviceToken(context.Background(), *region, registration.ClientID, registration.ClientSecret, deviceAuth.DeviceCode, deviceAuth.Interval, deviceAuth.ExpiresIn)
		if err != nil {
			done <- err
			return
		}
		creds.IdentityPoolID = strings.TrimSpace(*identityPoolID)
		creds.IdentityID = resolvedIdentityID
		if creds.IdentityID != "" {
			supplement, err := fetchKiroCognitoCredentials(context.Background(), http.DefaultClient, *region, creds.IdentityID)
			if err != nil {
				done <- err
				return
			}
			mergeKiroCredentials(&creds, supplement)
		}
		done <- saveKiroCredentialsMulti(strings.TrimSpace(*authDir), creds)
	}()

	select {
	case err := <-done:
		if err != nil {
			return err
		}
		fmt.Println("Kiro login completed successfully.")
		return nil
	case sig := <-quit:
		return fmt.Errorf("interrupted by %s", sig)
	}
}

type kiroOIDCRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type kiroDeviceAuthorization struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type kiroCognitoResponse struct {
	IdentityID  string `json:"IdentityId"`
	Credentials struct {
		AccessKeyID  string          `json:"AccessKeyId"`
		SecretKey    string          `json:"SecretKey"`
		SessionToken string          `json:"SessionToken"`
		Expiration   json.RawMessage `json:"Expiration"`
	} `json:"Credentials"`
}

func registerKiroOIDCClient(ctx context.Context, region string) (*kiroOIDCRegistration, error) {
	body := map[string]any{
		"clientName": "Kiro IDE",
		"clientType": "public",
		"scopes": []string{
			"codewhisperer:completions",
			"codewhisperer:analysis",
			"codewhisperer:conversations",
		},
	}
	var out kiroOIDCRegistration
	if err := doKiroJSONRequest(ctx, http.MethodPost, fmt.Sprintf("https://oidc.%s.amazonaws.com/client/register", region), body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ClientID) == "" || strings.TrimSpace(out.ClientSecret) == "" {
		return nil, fmt.Errorf("kiro oidc registration missing client credentials")
	}
	return &out, nil
}

func startKiroDeviceAuthorization(ctx context.Context, region, clientID, clientSecret, startURL string) (*kiroDeviceAuthorization, error) {
	body := map[string]any{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"startUrl":     startURL,
	}
	var out kiroDeviceAuthorization
	if err := doKiroJSONRequest(ctx, http.MethodPost, fmt.Sprintf("https://oidc.%s.amazonaws.com/device_authorization", region), body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.DeviceCode) == "" {
		return nil, fmt.Errorf("kiro device authorization missing deviceCode")
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out, nil
}

func pollKiroDeviceToken(ctx context.Context, region, clientID, clientSecret, deviceCode string, interval, expiresIn int) (kiroCredentials, error) {
	if interval <= 0 {
		interval = 5
	}
	maxAttempts := expiresIn / interval
	if maxAttempts <= 0 {
		maxAttempts = 60
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var out struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Error        string `json:"error"`
			Message      string `json:"message"`
		}
		err := doKiroJSONRequest(ctx, http.MethodPost, fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region), map[string]any{
			"clientId":     clientID,
			"clientSecret": clientSecret,
			"deviceCode":   deviceCode,
			"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
		}, &out)
		if err == nil && strings.TrimSpace(out.AccessToken) != "" {
			expiresAt := time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second)
			if out.ExpiresIn <= 0 {
				expiresAt = time.Now().UTC().Add(time.Hour)
			}
			return kiroCredentials{
				AccessToken:    out.AccessToken,
				RefreshToken:   out.RefreshToken,
				ClientID:       clientID,
				ClientSecret:   clientSecret,
				AuthMethod:     "builder-id",
				IDCRegion:      region,
				Region:         region,
				IdentityPoolID: kiroDefaultIdentityPool,
				ExpiresAt:      expiresAt.Format(time.RFC3339),
			}, nil
		}
		msg := strings.ToLower(strings.TrimSpace(out.Error + " " + out.Message))
		if strings.Contains(msg, "authorization_pending") {
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}
		if strings.Contains(msg, "slow_down") {
			time.Sleep(time.Duration(interval+5) * time.Second)
			continue
		}
		if err != nil {
			return kiroCredentials{}, err
		}
		return kiroCredentials{}, fmt.Errorf("kiro device token failed: %s", strings.TrimSpace(firstNonEmpty(out.Error, out.Message)))
	}
	return kiroCredentials{}, fmt.Errorf("kiro device authorization timed out")
}

func doKiroJSONRequest(ctx context.Context, method, endpoint string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "KiroIDE")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if out != nil && len(respBody) > 0 {
		_ = json.Unmarshal(respBody, out)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kiro request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func fetchKiroCognitoCredentials(ctx context.Context, client *http.Client, region, identityID string) (kiroCredentials, error) {
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return kiroCredentials{}, fmt.Errorf("kiro cognito identity id missing")
	}
	if client == nil {
		client = http.DefaultClient
	}
	payload, err := json.Marshal(map[string]any{
		"IdentityId": identityID,
	})
	if err != nil {
		return kiroCredentials{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://cognito-identity.%s.amazonaws.com/", region), strings.NewReader(string(payload)))
	if err != nil {
		return kiroCredentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityService.GetCredentialsForIdentity")
	req.Header.Set("User-Agent", "chimera/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return kiroCredentials{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return kiroCredentials{}, fmt.Errorf("kiro cognito credentials failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out kiroCognitoResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return kiroCredentials{}, err
	}
	if strings.TrimSpace(out.Credentials.AccessKeyID) == "" {
		return kiroCredentials{}, fmt.Errorf("kiro cognito credentials missing access key")
	}
	return kiroCredentials{
		IdentityID:           firstNonEmpty(strings.TrimSpace(out.IdentityID), identityID),
		AccessKeyID:          out.Credentials.AccessKeyID,
		SecretAccessKey:      out.Credentials.SecretKey,
		SessionToken:         out.Credentials.SessionToken,
		CredentialExpiration: parseKiroCredentialExpiration(out.Credentials.Expiration),
	}, nil
}

func parseKiroCredentialExpiration(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		secs := int64(f)
		if f > 1e12 {
			secs = int64(f / 1000)
		}
		if secs > 0 {
			return time.Unix(secs, 0).UTC().Format(time.RFC3339)
		}
	}
	return trimmed
}

func loadExistingKiroIdentity(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	creds, _, err := loadKiroCredentials(dir)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(creds.IdentityID)
}

func saveKiroCredentialsMulti(dir string, creds kiroCredentials) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = defaultKiroCLIAuthDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	primaryPath := filepath.Join(dir, kiroAuthTokenFile)
	if err := saveKiroCredentials(primaryPath, creds); err != nil {
		return err
	}
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	archivePath := filepath.Join(dir, fmt.Sprintf("%s_%s", timestamp, kiroAuthTokenFile))
	return saveKiroCredentials(archivePath, creds)
}

func openKiroBrowser(target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("empty url")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func runKiroImportCLIToken(args []string) error {
	fs := flag.NewFlagSet("kiro-import-cli-token", flag.ContinueOnError)
	authDir := fs.String("auth-dir", defaultKiroCLIAuthDir, "directory to store kiro token file")
	dbPath := fs.String("db-path", filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "kiro-cli", "data.sqlite3"), "path to kiro-cli sqlite database")
	key := fs.String("key", "social", "token type to import: social, odic, external-idp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sqliteKey := mapKiroCLIKey(*key)
	if sqliteKey == "" {
		return fmt.Errorf("unsupported kiro cli token key %q", *key)
	}

	raw, err := readKiroCLIAuthKV(strings.TrimSpace(*dbPath), sqliteKey)
	if err != nil {
		return err
	}
	creds, err := convertKiroCLIToken(sqliteKey, raw)
	if err != nil {
		return err
	}
	if err := saveKiroCredentialsMulti(strings.TrimSpace(*authDir), creds); err != nil {
		return err
	}
	fmt.Printf("Imported %s token to %s\n", sqliteKey, filepath.Join(strings.TrimSpace(*authDir), kiroAuthTokenFile))
	return nil
}

func mapKiroCLIKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "", "social":
		return "kirocli:social:token"
	case "odic", "builder-id", "builder":
		return "kirocli:odic:token"
	case "external-idp", "idp":
		return "kirocli:external-idp:token"
	default:
		return ""
	}
}

func readKiroCLIAuthKV(dbPath, key string) ([]byte, error) {
	dbPath = strings.TrimSpace(dbPath)
	key = strings.TrimSpace(key)
	if dbPath == "" {
		return nil, fmt.Errorf("kiro cli db path is required")
	}
	if key == "" {
		return nil, fmt.Errorf("kiro cli auth key is required")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, fmt.Errorf("sqlite3 not found in PATH")
	}
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("select value from auth_kv where key=%q;", key))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no value found for %s in %s", key, dbPath)
	}
	return out, nil
}

func convertKiroCLIToken(key string, raw []byte) (kiroCredentials, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return kiroCredentials{}, err
	}

	switch key {
	case "kirocli:social:token":
		creds := kiroCredentials{
			AccessToken:  toString(payload["access_token"]),
			RefreshToken: toString(payload["refresh_token"]),
			ProfileARN:   toString(payload["profile_arn"]),
			ExpiresAt:    toString(payload["expires_at"]),
			AuthMethod:   "social",
			Region:       kiroDefaultRegion,
		}
		if creds.AccessToken == "" || creds.RefreshToken == "" {
			return kiroCredentials{}, fmt.Errorf("invalid social token payload")
		}
		return creds, nil
	case "kirocli:odic:token":
		creds := kiroCredentials{
			AccessToken:  toString(payload["access_token"]),
			RefreshToken: toString(payload["refresh_token"]),
			ClientID:     toString(payload["client_id"]),
			ClientSecret: toString(payload["client_secret"]),
			ExpiresAt:    toString(payload["expires_at"]),
			AuthMethod:   "builder-id",
			IDCRegion:    firstNonEmpty(toString(payload["region"]), kiroDefaultRegion),
			Region:       firstNonEmpty(toString(payload["region"]), kiroDefaultRegion),
		}
		if creds.AccessToken == "" && creds.RefreshToken == "" {
			return kiroCredentials{}, fmt.Errorf("invalid builder-id token payload")
		}
		return creds, nil
	case "kirocli:external-idp:token":
		creds := kiroCredentials{
			AccessToken:  toString(payload["access_token"]),
			RefreshToken: toString(payload["refresh_token"]),
			ClientID:     toString(payload["client_id"]),
			ClientSecret: toString(payload["client_secret"]),
			ExpiresAt:    toString(payload["expires_at"]),
			AuthMethod:   "external-idp",
			IDCRegion:    firstNonEmpty(toString(payload["region"]), kiroDefaultRegion),
			Region:       firstNonEmpty(toString(payload["region"]), kiroDefaultRegion),
		}
		if creds.AccessToken == "" && creds.RefreshToken == "" {
			return kiroCredentials{}, fmt.Errorf("invalid external-idp token payload")
		}
		return creds, nil
	default:
		return kiroCredentials{}, fmt.Errorf("unsupported kiro cli key %q", key)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
