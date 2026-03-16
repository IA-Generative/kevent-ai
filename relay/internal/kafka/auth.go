package kafka

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"

	"kevent/relay/internal/config"
)

// buildTransport returns a Transport configured with SASL and/or TLS for Writers.
// Returns nil when neither is configured (kafka-go uses its default transport).
func buildTransport(cfg config.KafkaConfig) (*kafkago.Transport, error) {
	mechanism, err := buildSASL(cfg.SASL)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := buildTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	if mechanism == nil && tlsCfg == nil {
		return nil, nil
	}
	return &kafkago.Transport{
		SASL: mechanism,
		TLS:  tlsCfg,
	}, nil
}

func buildSASL(cfg config.SASLConfig) (sasl.Mechanism, error) {
	if cfg.Mechanism == "" {
		return nil, nil
	}
	switch strings.ToUpper(cfg.Mechanism) {
	case "PLAIN":
		return plain.Mechanism{Username: cfg.Username, Password: cfg.Password}, nil
	case "SCRAM-SHA-256":
		return scram.Mechanism(scram.SHA256, cfg.Username, cfg.Password)
	case "SCRAM-SHA-512":
		return scram.Mechanism(scram.SHA512, cfg.Username, cfg.Password)
	default:
		return nil, fmt.Errorf("unsupported SASL mechanism: %q", cfg.Mechanism)
	}
}

func buildTLS(cfg config.TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	tlsCfg := &tls.Config{}
	if cfg.CACertPath != "" {
		pem, err := os.ReadFile(cfg.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("reading kafka CA cert %q: %w", cfg.CACertPath, err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}
