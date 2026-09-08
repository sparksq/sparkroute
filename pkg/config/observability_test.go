package config_test

import (
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"path/filepath"
	"testing"
)

func TestObservabilityIsOperatorOwnedAndCloned(t *testing.T) {
	o := &config.Observability{SavedTraces: &config.SavedTraceConfig{Enabled: true, Storage: "filesystem", Path: filepath.Join(t.TempDir(), "traces")}}
	doc := managed.EmptyDocument()
	doc.Observability = o
	result, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerOperator: doc})
	if err != nil {
		t.Fatal(err)
	}
	o.SavedTraces.Enabled = false
	if !result.Observability.SavedTraces.Enabled {
		t.Fatal("merge aliases operator settings")
	}
	if _, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerSparkrun: doc}); err == nil {
		t.Fatal("generated owner controls trace export")
	}
}
func TestInvalidTraceSettingsRejected(t *testing.T) {
	for _, o := range []*config.Observability{
		{SavedTraces: &config.SavedTraceConfig{Enabled: true, Storage: "filesystem", Path: "relative"}},
		{SavedTraces: &config.SavedTraceConfig{QueueCapacity: 65537}},
		{OTLPTraces: &config.OTLPTraceConfig{Enabled: true, Endpoint: "https://user:secret@example.test/v1/traces"}},
		{OTLPTraces: &config.OTLPTraceConfig{Enabled: true, Endpoint: "https://example.test/v1/traces?token=secret"}},
		{OTLPTraces: &config.OTLPTraceConfig{HeaderEnv: map[string]string{"Authorization": "literal token"}}},
	} {
		if err := o.Validate(); err == nil {
			t.Fatal("accepted invalid trace settings")
		}
	}
}
