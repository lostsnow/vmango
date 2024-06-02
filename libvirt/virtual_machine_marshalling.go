package libvirt

import (
	"fmt"
	"strings"
	"subuk/vmango/compute"

	"github.com/libvirt/libvirt-go"
	libvirtxml "github.com/libvirt/libvirt-go-xml"
)

func DomainDiskConfigFromVirtualMachineAttachedVolume(volume *compute.VirtualMachineAttachedVolume, volTargetFormatType, volumeType string, namer *DeviceNamer) *libvirtxml.DomainDisk {
	diskDriverType := "raw"
	if volTargetFormatType == "qcow2" {
		diskDriverType = "qcow2"
	}

	diskConfig := &libvirtxml.DomainDisk{
		Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: diskDriverType},
		Target: &libvirtxml.DomainDiskTarget{},
	}
	if volume.Alias != "" {
		diskConfig.Alias = &libvirtxml.DomainAlias{Name: "ua-" + volume.Alias}
	}
	switch volume.DeviceType {
	default:
		panic(fmt.Errorf("unsupported volume device type '%s'", volume.DeviceType))
	case compute.DeviceTypeCdrom:
		diskConfig.Device = "cdrom"
		diskConfig.ReadOnly = &libvirtxml.DomainDiskReadOnly{}
		diskConfig.Target.Bus = volume.DeviceBus.String()
		diskConfig.Target.Dev = namer.Next(volume.DeviceBus)
	case compute.DeviceTypeDisk:
		diskConfig.Device = "disk"
		diskConfig.Target.Bus = volume.DeviceBus.String()
		diskConfig.Target.Dev = namer.Next(volume.DeviceBus)
	}
	switch volumeType {
	default:
		panic(fmt.Errorf("unknown volume type '%s'", volumeType))
	case "file":
		diskConfig.Source = &libvirtxml.DomainDiskSource{
			File: &libvirtxml.DomainDiskSourceFile{File: volume.Path},
		}
	case "block":
		diskConfig.Source = &libvirtxml.DomainDiskSource{
			Block: &libvirtxml.DomainDiskSourceBlock{Dev: volume.Path},
		}
	}

	return diskConfig
}

func VirtualMachineAttachedVolumeFromDomainDiskConfig(diskConfig libvirtxml.DomainDisk) *compute.VirtualMachineAttachedVolume {
	volume := &compute.VirtualMachineAttachedVolume{}
	volume.DeviceBus = compute.NewDeviceBus(diskConfig.Target.Bus)
	if diskConfig.Alias != nil {
		alias := diskConfig.Alias.Name
		if strings.HasPrefix(alias, "ua-") {
			volume.Alias = alias[3:]
		}
	}

	switch diskConfig.Device {
	default:
		volume.DeviceType = compute.DeviceTypeUnknown
	case "disk":
		volume.DeviceType = compute.DeviceTypeDisk
	case "cdrom":
		volume.DeviceType = compute.DeviceTypeCdrom
	}

	if diskConfig.Source != nil {
		if diskConfig.Source.File != nil {
			volume.Path = diskConfig.Source.File.File
		}
		if diskConfig.Source.Block != nil {
			volume.Path = diskConfig.Source.Block.Dev
		}
	}
	return volume
}

func VirtualMachineAttachedInterfaceFromInterfaceConfig(ifaceConfig libvirtxml.DomainInterface) *compute.VirtualMachineAttachedInterface {
	iface := &compute.VirtualMachineAttachedInterface{}
	iface.Mac = ifaceConfig.MAC.Address
	if ifaceConfig.Model != nil {
		iface.Model = ifaceConfig.Model.Type
	}
	if ifaceConfig.Source != nil {
		if ifaceConfig.Source.Network != nil {
			iface.NetworkName = ifaceConfig.Source.Network.Network
		}
		if ifaceConfig.Source.Bridge != nil {
			iface.NetworkName = ifaceConfig.Source.Bridge.Bridge
		}
	}

	if ifaceConfig.VLan != nil {
		if len(ifaceConfig.VLan.Tags) == 1 && ifaceConfig.VLan.Trunk == "" {
			iface.AccessVlan = ifaceConfig.VLan.Tags[0].ID
		}
	}
	return iface
}

func VirtualMachineFromDomainConfig(domainConfig *libvirtxml.Domain, domainInfo *libvirt.DomainInfo) (*compute.VirtualMachine, error) {
	vm := &compute.VirtualMachine{}
	vm.Id = domainConfig.Name
	vm.VCpus = int(domainConfig.VCPU.Value)
	vm.Memory = ComputeSizeFromLibvirtSize(domainConfig.Memory.Unit, uint64(domainConfig.Memory.Value))
	vm.Firmware = domainConfig.OS.Firmware

	switch domainConfig.OS.Type.Arch {
	default:
		vm.Arch = compute.ArchUnknown
	case "x86_64":
		vm.Arch = compute.ArchAmd64
	}

	switch domainInfo.State {
	default:
		vm.State = compute.StateUnknown
	case libvirt.DOMAIN_NOSTATE:
		vm.State = compute.StateUnknown
	case libvirt.DOMAIN_RUNNING:
		vm.State = compute.StateRunning
	case libvirt.DOMAIN_BLOCKED:
		vm.State = compute.StateStopped
	case libvirt.DOMAIN_PAUSED:
		vm.State = compute.StateStopped
	case libvirt.DOMAIN_SHUTDOWN:
		vm.State = compute.StateStopped
	case libvirt.DOMAIN_CRASHED:
		vm.State = compute.StateStopped
	case libvirt.DOMAIN_PMSUSPENDED:
		vm.State = compute.StateStopped
	case libvirt.DOMAIN_SHUTOFF:
		vm.State = compute.StateStopped
	}

	if domainConfig.CPUTune != nil {
		vm.Cpupin = &compute.VirtualMachineCpuPin{
			Vcpus:    map[uint][]uint{},
			Emulator: []uint{},
		}
		for _, vcpupin := range domainConfig.CPUTune.VCPUPin {
			vm.Cpupin.Vcpus[vcpupin.VCPU] = ParseCpuAffinity(vcpupin.CPUSet)
		}
		if domainConfig.CPUTune.EmulatorPin != nil {
			vm.Cpupin.Emulator = ParseCpuAffinity(domainConfig.CPUTune.EmulatorPin.CPUSet)
		}
	}

	for _, netInterfaceConfig := range domainConfig.Devices.Interfaces {
		iface := VirtualMachineAttachedInterfaceFromInterfaceConfig(netInterfaceConfig)
		vm.Interfaces = append(vm.Interfaces, iface)
	}

	for _, diskConfig := range domainConfig.Devices.Disks {
		volume := VirtualMachineAttachedVolumeFromDomainDiskConfig(diskConfig)
		vm.Volumes = append(vm.Volumes, volume)
	}
	for _, channel := range domainConfig.Devices.Channels {
		if channel.Target != nil && channel.Target.VirtIO != nil && channel.Target.VirtIO.Name == "org.qemu.guest_agent.0" {
			vm.GuestAgent = true
		}
	}

	if domainConfig.MemoryBacking != nil && domainConfig.MemoryBacking.MemoryHugePages != nil {
		vm.Hugepages = true
	}

	for _, graphic := range domainConfig.Devices.Graphics {
		if graphic.VNC != nil {
			vm.Graphic.Type = compute.GraphicTypeVnc
			vm.Graphic.Listen = graphic.VNC.Listen
			break
		}
		if graphic.Spice != nil {
			vm.Graphic.Type = compute.GraphicTypeSpice
			vm.Graphic.Listen = graphic.Spice.Listen
			break
		}
	}
	if vm.Graphic.Type == compute.GraphicTypeUnknown {
		vm.Graphic.Type = compute.GraphicTypeNone
	}

	for _, video := range domainConfig.Devices.Videos {
		switch video.Model.Type {
		case "qxl":
			vm.VideoModel = compute.VideoModelQxl
		case "cirrus":
			vm.VideoModel = compute.VideoModelCirrus
		}
	}
	if vm.VideoModel == compute.VideoModelUnknown {
		vm.VideoModel = compute.VideoModelNone
	}

	if domainConfig.QEMUCommandline != nil && len(domainConfig.QEMUCommandline.Args) > 0 {
		qargs := domainConfig.QEMUCommandline.Args
		qemuNetdevs := map[string]map[string]string{}

		for idx := 0; idx < len(qargs); idx++ {
			switch qargs[idx].Value {
			case "-netdev":
				idx++
				parts := strings.Split(qargs[idx].Value, ",")
				params := map[string]string{}
				params["type"] = parts[0]
				for _, part := range parts[1:] {
					kv := strings.SplitN(part, "=", 2)
					params[kv[0]] = kv[1]
				}
				if params["id"] == "" {
					continue
				}
				if _, ok := qemuNetdevs[params["id"]]; !ok {
					qemuNetdevs[params["id"]] = map[string]string{}
				}
				for k, v := range params {
					qemuNetdevs[params["id"]][k] = v
				}
			case "-device":
				idx++
				parts := strings.Split(qargs[idx].Value, ",")
				params := map[string]string{}
				params["model"] = parts[0]
				for _, part := range parts[1:] {
					kv := strings.SplitN(part, "=", 2)
					params[kv[0]] = kv[1]
				}
				if params["netdev"] == "" {
					continue
				}
				if _, ok := qemuNetdevs[params["netdev"]]; !ok {
					qemuNetdevs[params["netdev"]] = map[string]string{}
				}
				for k, v := range params {
					qemuNetdevs[params["netdev"]][k] = v
				}
			}
		}
		for netdevName, netdev := range qemuNetdevs {
			iface := &compute.VirtualMachineAttachedInterface{
				NetworkName: netdevName,
				Model:       netdev["model"],
				Mac:         netdev["mac"],
			}
			vm.Interfaces = append(vm.Interfaces, iface)
		}
	}

	return vm, nil
}
