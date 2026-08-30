package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/monirz/cloudrig/functions"
)

const defaultEndpoint = "http://localhost:4599"

// client talks to a running emulator's admin API.
type client struct{ endpoint string }

// resolveEndpoint takes the flag, then CLOUDRIG_ENDPOINT, then the default.
func resolveEndpoint(flagVal string, env lookupEnv) string {
	if flagVal != "" {
		return strings.TrimRight(flagVal, "/")
	}
	if v, ok := env("CLOUDRIG_ENDPOINT"); ok && v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultEndpoint
}

func (c client) deploy(ctx context.Context, r functions.DeployRequest) (functions.Descriptor, error) {
	var desc functions.Descriptor
	err := c.do(ctx, http.MethodPost, functions.AdminPath, r, &desc)
	return desc, err
}

func (c client) list(ctx context.Context, scope scope) ([]functions.Descriptor, error) {
	var body struct {
		Functions []functions.Descriptor `json:"functions"`
	}
	err := c.do(ctx, http.MethodGet, functions.AdminPath+scope.query(), nil, &body)
	return body.Functions, err
}

func (c client) describe(ctx context.Context, scope scope, name string) (functions.Descriptor, error) {
	var desc functions.Descriptor
	err := c.do(ctx, http.MethodGet, functions.AdminPath+"/"+name+scope.query(), nil, &desc)
	return desc, err
}

func (c client) delete(ctx context.Context, scope scope, name string) error {
	return c.do(ctx, http.MethodDelete, functions.AdminPath+"/"+name+scope.query(), nil, nil)
}

// scope narrows a request to a project and location.
type scope struct{ project, location string }

func (s scope) query() string {
	q := url.Values{}
	if s.project != "" {
		q.Set("project", s.project)
	}
	if s.location != "" {
		q.Set("location", s.location)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// do sends one admin request, turning the emulator's error envelope back into
// a Go error so the CLI reports what the server said rather than a status code.
func (c client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: is the emulator running? (cloudrig start)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", envelopeMessage(resp))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// envelopeMessage pulls the message out of a gerr envelope, falling back to the
// raw body when the response is not one.
func envelopeMessage(resp *http.Response) string {
	raw, _ := io.ReadAll(resp.Body)

	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	if len(raw) == 0 {
		return resp.Status
	}
	return strings.TrimSpace(string(raw))
}
