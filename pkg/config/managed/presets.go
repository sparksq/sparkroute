package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/config"
)

const DefaultPreset = "default"
const MaxPresets = 64

var ErrPresetNotFound = errors.New("configuration preset not found")
var ErrPresetConflict = errors.New("configuration presets changed; reload before trying again")
var ErrInvalidPreset = errors.New("invalid configuration preset")

type PresetMetadata struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Revision  config.Version `json:"revision"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type Preset struct {
	PresetMetadata
	Document config.Document `json:"document"`
}

type PresetCatalog struct {
	ActivePreset    string           `json:"active_preset"`
	PresetsRevision int64            `json:"presets_revision"`
	ActiveRevision  config.Version   `json:"active_revision"`
	Presets         []PresetMetadata `json:"presets"`
}

// PresetStore is optional: only the standalone mutable operator set has presets.
type PresetStore interface {
	ListPresets(context.Context) (PresetCatalog, error)
	GetPreset(context.Context, string) (Preset, error)
	SavePreset(context.Context, string, ReplaceOptions) (PresetMetadata, error)
	ActivatePreset(context.Context, string, ReplaceOptions) (ReplaceResult, error)
	RenamePreset(context.Context, string, string, ReplaceOptions) error
	DeletePreset(context.Context, string, ReplaceOptions) error
}

func ValidatePresetName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || len([]rune(name)) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: names must contain 1–64 characters, without leading or trailing whitespace or control characters", ErrInvalidPreset)
	}
	return nil
}
