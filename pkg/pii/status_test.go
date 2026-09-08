package pii

import (
	"context"
	"testing"
	"time"
)

type statusBackend struct {
	status MappingStoreStatus
	err    error
}

func (b *statusBackend) LoadPIIMappings(context.Context, ConversationScope, time.Time, ConversationLimits) ([]EncryptedMapping, error) {
	return nil, nil
}
func (b *statusBackend) PutPIIMappingIfAbsent(_ context.Context, mapping EncryptedMapping, _ time.Time, _ ConversationLimits) (EncryptedMapping, error) {
	return mapping, nil
}
func (b *statusBackend) RewrapPIIMapping(context.Context, EncryptedMapping, string, []byte, []byte) error {
	return nil
}
func (b *statusBackend) DeletePIIConversation(context.Context, ConversationScope) error { return nil }
func (b *statusBackend) PrunePIIMappings(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (b *statusBackend) PIIMappingStatus(context.Context, time.Time) (MappingStoreStatus, error) {
	return b.status, b.err
}

func TestMonitorPublishesContentFreeBackendAggregates(t *testing.T) {
	keyring, err := ParseKeyring([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	backend := &statusBackend{status: MappingStoreStatus{
		Backend: "sqlite", LiveMappings: 7, LiveConversations: 2,
		LiveOriginalBytes: 128, ExpiredPendingPrune: 1,
	}}
	directory, err := NewDirectory(backend, keyring, DirectoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitor(MonitorOptions{
		ConfiguredModels: 2, ConversationModels: 1, MediaRequiredModels: 1,
		FileInspectionModels: 1, Detector: NewBuiltinDetector(), Directory: directory,
	})
	monitor.RecordInspection()
	monitor.RecordFailure()
	status := monitor.PrivacyStatus(t.Context())
	if status.State != StatusHealthy || status.Backend != "sqlite" ||
		status.LiveMappings != 7 || status.LiveConversations != 2 ||
		status.MaximumMappings != DefaultMaximumConversationMappings ||
		status.SlidingTTLSeconds != int64(DefaultConversationSlidingTTL/time.Second) ||
		status.Inspections != 1 || status.Failures != 1 ||
		len(status.DetectorEntities) != len(DefaultEntities()) {
		t.Fatalf("PrivacyStatus() = %#v", status)
	}
}
