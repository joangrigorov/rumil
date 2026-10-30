package config

import "testing"

// The separate listener is opt-in, and an address a scraper cannot discover is
// refused at load rather than at bind.
func TestMetricsListenMustBeAHostAndPort(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding+`
server:
  metrics_listen: "127.0.0.1:9090"
`)
	if cfg.Server.MetricsListen != "127.0.0.1:9090" {
		t.Errorf("server.metrics_listen = %q", cfg.Server.MetricsListen)
	}
	if cfg := mustParse(t, twoChatOneEmbedding); cfg.Server.MetricsListen != "" {
		t.Errorf("server.metrics_listen defaults to %q, want empty", cfg.Server.MetricsListen)
	}

	if _, err := Parse([]byte(twoChatOneEmbedding+`
server:
  metrics_listen: "not-an-address"
`), "test.yaml"); err == nil {
		t.Fatal("Parse accepted server.metrics_listen without a port")
	}
}
