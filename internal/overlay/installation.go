package overlay

import (
	"errors"
	"os"

	"gitlocal/internal/storage"
)

const installationFile = "overlay-install.json"

// installation remembers only state that cannot be inferred from the managed
// attributes block: whether the file existed, its mode, and the original
// extension-owned include/config bytes. User attribute content stays in place.
type installation struct {
	SchemaVersion      int    `json:"schemaVersion"`
	AttributesExisted  bool   `json:"attributesExisted"`
	AttributesMode     uint32 `json:"attributesMode"`
	ConfigExisted      bool   `json:"configExisted"`
	ConfigMode         uint32 `json:"configMode"`
	ConfigData         []byte `json:"configData,omitempty"`
	IncludePathExisted bool   `json:"includePathExisted"`
	RequiredExisted    bool   `json:"requiredExisted,omitempty"`
}

func loadInstallation(d *storage.Dir) (installation, bool, error) {
	var record installation
	err := d.ReadJSON(installationFile, &record)
	if os.IsNotExist(err) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	if record.SchemaVersion != 1 || record.AttributesMode > 0777 || record.ConfigMode > 0777 || (!record.ConfigExisted && len(record.ConfigData) != 0) {
		return installation{}, false, errors.New("invalid overlay installation record")
	}
	return record, true, nil
}

// loadInstallationForDeinit is loadInstallation for git local deinit, which
// removes the filter as installed by git-local when the record is missing:
// a record it can't read (damaged, or from a newer release) is taken as
// missing too, and problem says why (GL-121).
func loadInstallationForDeinit(d *storage.Dir) (record installation, exists bool, problem string) {
	record, exists, err := loadInstallation(d)
	if err != nil {
		return installation{}, false, "couldn't be read (" + err.Error() + ")"
	}
	if !exists {
		return record, false, "was missing"
	}
	return record, true, ""
}
