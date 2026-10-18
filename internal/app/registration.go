package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"stream-gateway-lab/internal/control"
	"stream-gateway-lab/internal/endpoint"
)

type registration struct {
	Gateway string `json:"gateway"`
	ID      string `json:"id"`
	Token   string `json:"token"`
	Name    string `json:"name"`
}

func gatewayURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gateway must be an http(s) origin, for example https://gateway.example.com")
	}
	return u, nil
}

func readRegistration(path string) (registration, error) {
	var result registration
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if result.ID == "" || result.Token == "" {
		return result, errors.New("invalid registration file")
	}
	_, err = gatewayURL(result.Gateway)
	return result, err
}

func registerExecutor(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("executor register", flag.ContinueOnError)
	address := flags.String("gateway", "", "Gateway HTTP(S) origin")
	name := flags.String("name", "", "display name; defaults to hostname")
	path := flags.String("out", "executor.json", "private registration file")
	tokenEnv := flags.String("token-env", "LAB_JOIN_TOKEN", "environment variable holding one-use registration code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	u, err := gatewayURL(*address)
	if err != nil {
		return err
	}
	if saved, err := readRegistration(*path); err == nil {
		if saved.Gateway != u.String() {
			return errors.New("registration belongs to another Gateway; use a new output path")
		}
		log.Printf("reusing executor registration id=%s name=%s", saved.ID, saved.Name)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	token := os.Getenv(*tokenEnv)
	if token == "" {
		return errors.New("missing one-use registration code")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	if *name == "" {
		*name = hostname
	}
	data, _ := json.Marshal(map[string]string{"name": *name, "hostname": hostname, "os": runtime.GOOS, "arch": runtime.GOARCH})
	req, err := http.NewRequestWithContext(ctx, "POST", u.String()+"/api/register", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("Gateway must not redirect registration")
	}}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		var body struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &body)
		return fmt.Errorf("registration failed (%d): %s", response.StatusCode, body.Error)
	}
	var saved registration
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if saved.ID == "" || saved.Token == "" {
		return errors.New("Gateway returned invalid credentials")
	}
	saved.Gateway, saved.Name = u.String(), *name
	data, _ = json.MarshalIndent(saved, "", "  ")
	if err := control.WritePrivateFile(*path, data); err != nil {
		return err
	}
	log.Printf("registered executor id=%s name=%s config=%s", saved.ID, saved.Name, *path)
	return nil
}

func runRegisteredExecutor(ctx context.Context, path string) error {
	saved, err := readRegistration(path)
	if err != nil {
		return err
	}
	u, _ := gatewayURL(saved.Gateway)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	c, err := endpoint.NewPoolClient(ctx, u.String(), saved.ID, saved.Token)
	if err != nil {
		return err
	}
	log.Printf("pool executor id=%s name=%s gateway=%s", saved.ID, saved.Name, saved.Gateway)
	return endpoint.RunExecutorClient(ctx, c)
}
