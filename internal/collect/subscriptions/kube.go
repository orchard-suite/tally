package subscriptions

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

// kube is the Kubernetes API as the pod's service account sees it. The
// probe only ever reads and writes the secret that holds the active build
// account and the config map that holds the account picked by hand.
type kube struct {
	base      string
	namespace string
	token     string
	http      *http.Client
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// inCluster builds the client from the service account the pod mounts.
func inCluster() (*kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST is unset")
	}
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, err
	}
	ns, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA has no certificate")
	}
	return &kube{
		base:      "https://" + net.JoinHostPort(host, port),
		namespace: strings.TrimSpace(string(ns)),
		token:     strings.TrimSpace(string(token)),
		http:      &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

type secretObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data"`
}

func (k *kube) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := k.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, raw, err
}

func (k *kube) secretPath(name string) string {
	return "/api/v1/namespaces/" + k.namespace + "/secrets/" + name
}

// secretValue returns one key of a secret, and false when the secret or the
// key is absent.
func (k *kube) secretValue(ctx context.Context, name, key string) ([]byte, bool, error) {
	code, raw, err := k.do(ctx, http.MethodGet, k.secretPath(name), nil)
	if err != nil {
		return nil, false, err
	}
	if code == http.StatusNotFound {
		return nil, false, nil
	}
	if code != http.StatusOK {
		return nil, false, fmt.Errorf("get secret %s: HTTP %d: %s", name, code, bytes.TrimSpace(raw))
	}
	var s secretObject
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false, err
	}
	enc, ok := s.Data[key]
	if !ok {
		return nil, false, nil
	}
	v, err := base64.StdEncoding.DecodeString(enc)
	return v, err == nil, err
}

// putSecret creates the secret with one key, or replaces it.
func (k *kube) putSecret(ctx context.Context, name, key string, value []byte) error {
	obj := secretObject{
		APIVersion: "v1", Kind: "Secret", Type: "Opaque",
		Metadata: map[string]any{"name": name, "namespace": k.namespace, "labels": map[string]string{"app.kubernetes.io/managed-by": "usage-probe"}},
		Data:     map[string]string{key: base64.StdEncoding.EncodeToString(value)},
	}
	code, raw, err := k.do(ctx, http.MethodPut, k.secretPath(name), obj)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		code, raw, err = k.do(ctx, http.MethodPost, "/api/v1/namespaces/"+k.namespace+"/secrets", obj)
		if err != nil {
			return err
		}
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("put secret %s: HTTP %d: %s", name, code, bytes.TrimSpace(raw))
	}
	return nil
}

type configMapObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func (k *kube) configMapPath(name string) string {
	return "/api/v1/namespaces/" + k.namespace + "/configmaps/" + name
}

// configMapValue returns one key of a config map; empty when the map or the
// key is absent.
func (k *kube) configMapValue(ctx context.Context, name, key string) (string, error) {
	code, raw, err := k.do(ctx, http.MethodGet, k.configMapPath(name), nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", nil
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("get config map %s: HTTP %d: %s", name, code, bytes.TrimSpace(raw))
	}
	var m configMapObject
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	return m.Data[key], nil
}

// putConfigMap creates the config map with one key, or replaces it.
func (k *kube) putConfigMap(ctx context.Context, name, key, value string) error {
	obj := configMapObject{
		APIVersion: "v1", Kind: "ConfigMap",
		Metadata: map[string]any{"name": name, "namespace": k.namespace, "labels": map[string]string{"app.kubernetes.io/managed-by": "usage-probe"}},
		Data:     map[string]string{key: value},
	}
	code, raw, err := k.do(ctx, http.MethodPut, k.configMapPath(name), obj)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		code, raw, err = k.do(ctx, http.MethodPost, "/api/v1/namespaces/"+k.namespace+"/configmaps", obj)
		if err != nil {
			return err
		}
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("put config map %s: HTTP %d: %s", name, code, bytes.TrimSpace(raw))
	}
	return nil
}

// ensureConfigMap creates the config map with an empty key when it is absent,
// so others can patch it, and leaves one that exists alone.
func (k *kube) ensureConfigMap(ctx context.Context, name, key string) error {
	code, raw, err := k.do(ctx, http.MethodGet, k.configMapPath(name), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return k.putConfigMap(ctx, name, key, "")
	}
	return fmt.Errorf("get config map %s: HTTP %d: %s", name, code, bytes.TrimSpace(raw))
}
