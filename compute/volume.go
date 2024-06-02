package compute

import (
	"path/filepath"
)

type VolumeMetadata struct {
	OsName    string
	OsVersion string
	OsArch    Arch
	Protected bool
	Hidden    bool
	Efi       bool
}

type Volume struct {
	NodeId     string
	Path       string
	Name       string
	Size       Size
	Pool       string
	Format     VolumeFormat
	AttachedTo string
	AttachedAs DeviceType
	Metadata   VolumeMetadata
}

func (volume *Volume) Base() string {
	return filepath.Base(volume.Path)
}
