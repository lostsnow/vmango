package libvirt

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"subuk/vmango/compute"
	"subuk/vmango/configdrive"
	"subuk/vmango/util"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/google/uuid"
	"github.com/libvirt/libvirt-go"
	libvirtxml "github.com/libvirt/libvirt-go-xml"
	"github.com/rs/zerolog"
)

const ConfigDriveMaxSize = 5 * 1024 * 1024
const VirtualMachineSockets = 1

type VirtualMachineRepository struct {
	pool          *ConnectionPool
	logger        zerolog.Logger
	settings      map[string]NodeSettings
	configCache   map[string]*compute.VirtualMachineConfig
	configCacheMu *sync.Mutex
}

func NewVirtualMachineRepository(pool *ConnectionPool, settings map[string]NodeSettings, logger zerolog.Logger) *VirtualMachineRepository {
	return &VirtualMachineRepository{
		pool:          pool,
		settings:      settings,
		logger:        logger,
		configCache:   map[string]*compute.VirtualMachineConfig{},
		configCacheMu: &sync.Mutex{},
	}
}

type virStreamReader struct {
	*libvirt.Stream
}

func (r *virStreamReader) Read(b []byte) (int, error) {
	return r.Recv(b)
}

func (repo *VirtualMachineRepository) parseConfigDrive(conn *libvirt.Connect, volumePath string) (*compute.VirtualMachineConfig, error) {
	virVolume, err := conn.LookupStorageVolByPath(volumePath)
	if err != nil {
		return nil, util.NewError(err, "cannot lookup volume")
	}
	stream, err := conn.NewStream(0)
	if err != nil {
		return nil, util.NewError(err, "cannot initialize configdrive download stream")
	}
	if err := virVolume.Download(stream, 0, ConfigDriveMaxSize, 0); err != nil {
		return nil, util.NewError(err, "cannot start configdrive download")
	}
	data, err := configdrive.ParseIso(configdrive.AllFormats, &virStreamReader{stream})
	if err != nil {
		return nil, util.NewError(err, "cannot parse configdrive iso")
	}
	config := &compute.VirtualMachineConfig{
		Hostname: data.Hostname(),
	}
	for _, rawKey := range data.PublicKeys() {
		pubkey, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(rawKey))
		if err != nil {
			repo.logger.Warn().Msg("ignoring invalid ssh key")
			continue
		}
		key := &compute.Key{
			Type:        pubkey.Type(),
			Value:       []byte(rawKey),
			Comment:     comment,
			Options:     options,
			Fingerprint: ssh.FingerprintLegacyMD5(pubkey),
		}
		config.Keys = append(config.Keys, key)
	}
	return config, nil
}

func (repo *VirtualMachineRepository) domainToVm(conn *libvirt.Connect, nodeId string, domain *libvirt.Domain, settings NodeSettings) (*compute.VirtualMachine, error) {
	domainXml, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return nil, util.NewError(err, "cannot get domain xml")
	}
	domainConfig := &libvirtxml.Domain{}
	if err := domainConfig.Unmarshal(domainXml); err != nil {
		return nil, util.NewError(err, "cannot unmarshal domain xml")
	}
	domainInfo, err := domain.GetInfo()
	if err != nil {
		return nil, util.NewError(err, "cannot get domain info")
	}
	vm, err := VirtualMachineFromDomainConfig(domainConfig, domainInfo)
	if err != nil {
		return nil, util.NewError(err, "cannot create virtual machine from domain config")
	}

	autostart, err := domain.GetAutostart()
	if err != nil {
		return nil, util.NewError(err, "cannot get domain autostart value")
	}
	vm.Autostart = autostart

	vm.NodeId = nodeId

	if vm.IsRunning() && len(vm.Interfaces) > 0 {
		virDomainIfaces := []libvirt.DomainInterface{}
		if vm.GuestAgent {
			ifaces, err := domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT)
			if err != nil {
				repo.logger.Debug().Str("vm", vm.Id).Err(err).Msg("cannot get interfaces addresses from guest agent")
			}
			virDomainIfaces = ifaces
		}
		if len(virDomainIfaces) <= 0 {
			ifaces, err := domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE)
			if err != nil {
				repo.logger.Debug().Str("vm", vm.Id).Err(err).Msg("cannot get interfaces addresses from dhcp leases")
			}
			virDomainIfaces = ifaces
		}
		if len(virDomainIfaces) <= 0 {
			ifaces, err := domain.ListAllInterfaceAddresses(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_ARP)
			if err != nil {
				repo.logger.Debug().Str("vm", vm.Id).Err(err).Msg("cannot get interfaces addresses from arp tables")
			}
			virDomainIfaces = ifaces
		}
		for _, virDomainIface := range virDomainIfaces {
			for _, attachedInterface := range vm.Interfaces {
				if strings.EqualFold(virDomainIface.Hwaddr, attachedInterface.Mac) {
					for _, addr := range virDomainIface.Addrs {
						attachedInterface.IpAddressList = append(attachedInterface.IpAddressList, addr.Addr)
					}
				}
			}
		}
	}

	for _, volume := range vm.Volumes {
		if !strings.HasSuffix(volume.Path, settings.CdSuffix) {
			continue
		}
		if config := repo.configCache[volume.Path]; config != nil {
			vm.Config = config
			continue
		}

		config, err := repo.parseConfigDrive(conn, volume.Path)
		if err != nil {
			repo.logger.Warn().Err(err).Str("volume_path", volume.Path).Msg("cannot parse configdrive")
			continue
		}
		vm.Config = config
		repo.configCacheMu.Lock()
		repo.configCache[volume.Path] = config
		repo.configCacheMu.Unlock()
	}

	return vm, nil
}

func (repo *VirtualMachineRepository) attachInterface(virDomainConfig *libvirtxml.Domain, attachedIface *compute.VirtualMachineAttachedInterface) error {
	if attachedIface.NetworkName == "macos-socket-vmnet" || attachedIface.NetworkName == "qemu-usernet" {
		if virDomainConfig.QEMUCommandline == nil {
			virDomainConfig.QEMUCommandline = &libvirtxml.DomainQEMUCommandline{}
		}
		if attachedIface.Mac == "" {
			attachedIface.Mac = generateMacAddress("52:54:00")
		}

		ifaceNum := 0
		for idx, arg := range virDomainConfig.QEMUCommandline.Args {
			if arg.Value != "-netdev" {
				continue
			}
			argv := virDomainConfig.QEMUCommandline.Args[idx+1].Value
			params := parseQemuDeviceArg(argv)
			if strings.HasPrefix(params["id"], "vmngmcos") {
				thisIfaceNum, err := strconv.ParseInt(string(params["id"][len("vmngmcos"):]), 10, 64)
				if err != nil {
					repo.logger.Debug().Err(err).Str("full_argv", argv).Msg("failed to extract number from vmngmcos interface")
					continue
				}
				if thisIfaceNum > int64(ifaceNum) {
					ifaceNum = int(thisIfaceNum)
				}
			}
		}
		ifaceNum++
		ifaceName := fmt.Sprintf("vmngmcos%d", ifaceNum)
		switch attachedIface.NetworkName {
		default:
			panic(fmt.Errorf("unhandled network type %s", attachedIface.NetworkName))
		case "macos-socket-vmnet":
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "-netdev"})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "socket,id=" + ifaceName + ",fd=3"})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "-device"})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "virtio-net-device,netdev=" + ifaceName + ",mac=" + attachedIface.Mac})
		case "qemu-usernet":
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "-netdev"})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "user,id=" + ifaceName})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "-device"})
			virDomainConfig.QEMUCommandline.Args = append(virDomainConfig.QEMUCommandline.Args, libvirtxml.DomainQEMUCommandlineArg{Value: "virtio-net-device,netdev=" + ifaceName + ",mac=" + attachedIface.Mac})
		}

		return nil
	}

	if attachedIface.Model == "" {
		attachedIface.Model = "virtio"
	}
	domainIface := libvirtxml.DomainInterface{}
	if attachedIface.Mac != "" {
		domainIface.MAC = &libvirtxml.DomainInterfaceMAC{Address: attachedIface.Mac}
	}
	domainIface.Source = &libvirtxml.DomainInterfaceSource{}
	domainIface.Model = &libvirtxml.DomainInterfaceModel{Type: attachedIface.Model}
	domainIface.Source.Network = &libvirtxml.DomainInterfaceSourceNetwork{
		Network: attachedIface.NetworkName,
	}
	if attachedIface.AccessVlan > 0 {
		domainIface.VLan = &libvirtxml.DomainInterfaceVLan{
			Tags: []libvirtxml.DomainInterfaceVLanTag{libvirtxml.DomainInterfaceVLanTag{ID: attachedIface.AccessVlan}},
		}
	}
	virDomainConfig.Devices.Interfaces = append(virDomainConfig.Devices.Interfaces, domainIface)
	return nil
}

type virStreamReadWriteCloser struct {
	*libvirt.Stream
}

func (r *virStreamReadWriteCloser) Read(b []byte) (int, error) {
	return r.Recv(b)
}

func (r *virStreamReadWriteCloser) Write(b []byte) (int, error) {
	return r.Send(b)
}

func (r *virStreamReadWriteCloser) Close() error {
	return r.Stream.Finish()
}

func (repo *VirtualMachineRepository) GetConsoleStream(id, nodeId string) (compute.VirtualMachineConsoleStream, error) {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return nil, util.NewError(err, "cannot acquire libvirt connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return nil, util.NewError(err, "cannot get vm")
	}
	stream, err := conn.NewStream(0)
	if err != nil {
		return nil, util.NewError(err, "cannot create stream")
	}
	if err := virDomain.OpenConsole("", stream, libvirt.DOMAIN_CONSOLE_FORCE); err != nil {
		return nil, util.NewError(err, "cannot open domain console")
	}
	return &virStreamReadWriteCloser{stream}, nil
}

type cmdIoWrapper struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (w *cmdIoWrapper) Read(p []byte) (int, error) {
	return w.stdout.Read(p)
}

func (w *cmdIoWrapper) Write(p []byte) (int, error) {
	return w.stdin.Write(p)
}

func (w *cmdIoWrapper) Close() error {
	w.stdin.Close()
	return w.stdout.Close()

}

func (repo *VirtualMachineRepository) GetGraphicStream(id, nodeId string) (compute.VirtualMachineGraphicStream, error) {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return nil, util.NewError(err, "cannot acquire libvirt connection")
	}
	defer repo.pool.Release(nodeId)

	connUriRaw, err := conn.GetURI()
	if err != nil {
		return nil, util.NewError(err, "cannot get connection hostname")
	}
	connUri, err := url.Parse(connUriRaw)
	if err != nil {
		return nil, util.NewError(err, "cannot parse connection uri")
	}
	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return nil, util.NewError(err, "cannot get vm")
	}
	virDomainRunning, err := virDomain.IsActive()
	if err != nil {
		return nil, util.NewError(err, "cannot check if domain is running")
	}
	if !virDomainRunning {
		return nil, fmt.Errorf("domain is not running")
	}
	virDomainXml, err := virDomain.GetXMLDesc(0)
	if err != nil {
		return nil, util.NewError(err, "cannot fetch domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return nil, util.NewError(err, "cannot parse domain xml")
	}
	graphicPort := 0
	for _, graphic := range virDomainConfig.Devices.Graphics {
		if graphic.VNC != nil {
			graphicPort = graphic.VNC.Port
		}
	}
	if graphicPort <= 0 {
		return nil, fmt.Errorf("no graphic port found")
	}

	if strings.Contains(connUri.Scheme, "ssh") {
		portStr := fmt.Sprintf("%d", graphicPort)
		args := []string{
			"ssh", "-l", connUri.User.Username(), connUri.Hostname(),
			"if (command -v socat) >/dev/null 2>&1; then socat - TCP:localhost:" + portStr + "; else nc localhost " + portStr + "; fi",
		}
		cmd := exec.Command(args[0], args[1:]...)
		repo.logger.Debug().Strs("args", cmd.Args).Msg("executing ssh forwarding command")

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, util.NewError(err, "cannot initialize cmd stdout")
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, util.NewError(err, "cannot initialize cmd stdin")
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, util.NewError(err, "cannot initialize cmd stdin")
		}
		if err := cmd.Start(); err != nil {
			return nil, util.NewError(err, "cannot start ssh forwarding command")
		}
		repo.logger.Debug().Msg("ssh started successfully")
		go func(cmd *exec.Cmd) {
			if err := cmd.Wait(); err != nil {
				errText, _ := ioutil.ReadAll(stderr)
				repo.logger.Warn().Err(err).Str("text", string(errText)).Msg("graphics forwarding command failed")
				return
			}
			repo.logger.Debug().Msg("graphics command finished successfully")
		}(cmd)
		return &cmdIoWrapper{stdin: stdin, stdout: stdout}, nil
	}
	addr := fmt.Sprintf("%s:%d", connUri.Hostname(), graphicPort)
	repo.logger.Debug().Str("addr", addr).Msg("connecting directly to console")
	return net.Dial("tcp", addr)
}

func (repo *VirtualMachineRepository) generateNewDomainConfig(conn *libvirt.Connect, vm *compute.VirtualMachine) (*libvirtxml.Domain, error) {
	settings := repo.settings[vm.NodeId]
	domCapsXml, err := conn.GetDomainCapabilities(settings.Emulator, vm.Arch.String(), "", "", 0)
	if err != nil {
		return nil, util.NewError(err, "cannot fetch domain capabilities")
	}
	domCapsConfig := &libvirtxml.DomainCaps{}
	if err := domCapsConfig.Unmarshal(domCapsXml); err != nil {
		return nil, util.NewError(err, "cannot parse domain capabilities")
	}

	virDomainConfig := &libvirtxml.Domain{}
	virDomainConfig.UUID = uuid.New().String()
	virDomainConfig.Type = domCapsConfig.Domain
	virDomainConfig.Name = vm.Id
	virDomainConfig.OS = &libvirtxml.DomainOS{
		Type: &libvirtxml.DomainOSType{Type: "hvm", Machine: domCapsConfig.Machine, Arch: domCapsConfig.Arch},
		BootDevices: []libvirtxml.DomainBootDevice{
			{Dev: "cdrom"},
			{Dev: "hd"},
		},
	}
	if vm.Firmware != "" {
		virDomainConfig.OS.Firmware = vm.Firmware
	}
	virDomainConfig.CPU = &libvirtxml.DomainCPU{}
	virDomainConfig.CPU.Mode = "maximum"
	virDomainConfig.Clock = &libvirtxml.DomainClock{Offset: "utc"}
	virDomainConfig.OnPoweroff = "destroy"
	virDomainConfig.OnReboot = "restart"
	virDomainConfig.OnCrash = "destroy"
	virDomainConfig.Devices = &libvirtxml.DomainDeviceList{Emulator: domCapsConfig.Path}
	virDomainConfig.Devices.Consoles = append(virDomainConfig.Devices.Consoles, libvirtxml.DomainConsole{})

	return virDomainConfig, nil
}

func (repo *VirtualMachineRepository) Save(vm *compute.VirtualMachine) error {
	conn, err := repo.pool.Acquire(vm.NodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire libvirt connection")
	}
	defer repo.pool.Release(vm.NodeId)

	capsXml, err := conn.GetCapabilities()
	if err != nil {
		return util.NewError(err, "cannot fetch domain capabilities")
	}
	capsConfig := &libvirtxml.Caps{}
	if err := capsConfig.Unmarshal(capsXml); err != nil {
		return util.NewError(err, "cannot parse capabilities")
	}

	var isNewVm bool
	var virDomainConfig *libvirtxml.Domain

	existingVirDomain, err := conn.LookupDomainByName(vm.Id)
	if err != nil {
		if lErr, ok := err.(*libvirt.Error); ok {
			if lErr.Code != libvirt.ERR_NO_DOMAIN {
				return util.NewError(err, "cannot lookup domain")
			}
		}
		isNewVm = true
	}

	if isNewVm {
		newVirDomainConfig, err := repo.generateNewDomainConfig(conn, vm)
		if err != nil {
			return util.NewError(err, "cannot generate new domain config")
		}
		virDomainConfig = newVirDomainConfig
	} else {
		virDomainXml, err := existingVirDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
		if err != nil {
			return util.NewError(err, "cannot fetch xml")
		}
		virDomainConfig = &libvirtxml.Domain{}
		if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
			return util.NewError(err, "cannot parse domain xml")
		}
	}

	virDomainConfig.VCPU = &libvirtxml.DomainVCPU{Placement: "static", Value: uint(vm.VCpus)}
	virDomainConfig.Memory = &libvirtxml.DomainMemory{Unit: "bytes", Value: uint(vm.Memory.Bytes())}

	if vm.Hugepages {
		virDomainConfig.MemoryBacking = &libvirtxml.DomainMemoryBacking{
			MemoryHugePages: &libvirtxml.DomainMemoryHugepages{},
		}
	} else {
		if virDomainConfig.MemoryBacking != nil && virDomainConfig.MemoryBacking.MemoryHugePages != nil {
			virDomainConfig.MemoryBacking.MemoryHugePages = nil
		}
	}

	if capsConfig.Host.CPU != nil && capsConfig.Host.CPU.Topology != nil && capsConfig.Host.CPU.Topology.Threads > 0 {
		threadsPerCore := capsConfig.Host.CPU.Topology.Threads
		if vm.VCpus%threadsPerCore == 0 {
			virDomainConfig.CPU.Topology = &libvirtxml.DomainCPUTopology{
				Sockets: VirtualMachineSockets,
				Cores:   vm.VCpus / threadsPerCore,
				Threads: threadsPerCore,
			}
		} else {
			virDomainConfig.CPU.Topology = nil
		}
	}

	if vm.GuestAgent {
		hasGuestAgent := false
		for _, channel := range virDomainConfig.Devices.Channels {
			if channel.Target != nil && channel.Target.VirtIO != nil && channel.Target.VirtIO.Name == "org.qemu.guest_agent.0" {
				hasGuestAgent = true
				break
			}
		}
		if !hasGuestAgent {
			virDomainConfig.Devices.Channels = append(virDomainConfig.Devices.Channels, libvirtxml.DomainChannel{
				Protocol: &libvirtxml.DomainChardevProtocol{Type: "unix"},
				Target:   &libvirtxml.DomainChannelTarget{VirtIO: &libvirtxml.DomainChannelTargetVirtIO{Name: "org.qemu.guest_agent.0"}},
				Source:   &libvirtxml.DomainChardevSource{UNIX: &libvirtxml.DomainChardevSourceUNIX{}},
			})
		}
	} else {
		newChannels := []libvirtxml.DomainChannel{}
		for _, channel := range virDomainConfig.Devices.Channels {
			if channel.Target != nil && channel.Target.VirtIO != nil && channel.Target.VirtIO.Name == "org.qemu.guest_agent.0" {
				continue
			}
			newChannels = append(newChannels, channel)
		}
		virDomainConfig.Devices.Channels = newChannels
	}

	switch vm.Graphic.Type {
	default:
		panic("unknown graphic type")
	case compute.GraphicTypeNone:
		virDomainConfig.Devices.Graphics = nil
	case compute.GraphicTypeVnc:
		vncGraphic := &libvirtxml.DomainGraphicVNC{Port: -1, AutoPort: "yes", Listen: vm.Graphic.Listen}
		virDomainConfig.Devices.Graphics = []libvirtxml.DomainGraphic{
			libvirtxml.DomainGraphic{
				VNC: vncGraphic,
			},
		}
	case compute.GraphicTypeSpice:
		spiceGraphic := &libvirtxml.DomainGraphicSpice{Port: -1, AutoPort: "yes", Listen: vm.Graphic.Listen}
		virDomainConfig.Devices.Graphics = []libvirtxml.DomainGraphic{
			libvirtxml.DomainGraphic{
				Spice: spiceGraphic,
			},
		}
	}

	switch vm.VideoModel {
	default:
		panic("unknown video model")
	case compute.VideoModelQxl:
		virDomainConfig.Devices.Videos = []libvirtxml.DomainVideo{
			libvirtxml.DomainVideo{Model: libvirtxml.DomainVideoModel{Type: "qxl"}},
		}
	case compute.VideoModelCirrus:
		virDomainConfig.Devices.Videos = []libvirtxml.DomainVideo{
			libvirtxml.DomainVideo{Model: libvirtxml.DomainVideoModel{Type: "cirrus"}},
		}
	case compute.VideoModelNone:
		virDomainConfig.Devices.Videos = nil
	}

	if isNewVm {
		namer := NewDeviceNamer()
		for _, attachedVolume := range vm.Volumes {
			if err := repo.attachVolume(conn, virDomainConfig, attachedVolume, namer); err != nil {
				return util.NewError(err, "cannot attach volume")
			}
		}
		for _, attachedIface := range vm.Interfaces {
			if err := repo.attachInterface(virDomainConfig, attachedIface); err != nil {
				return util.NewError(err, "cannot attach interface")
			}
		}
	}
	virDomainXml, err := virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot marshal domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		fmt.Println(virDomainXml)
		return util.NewError(err, "cannot define new domain")
	}
	virDomain, err := conn.LookupDomainByName(vm.Id)
	if err != nil {
		return util.NewError(err, "cannot lookup domain after define")
	}
	virDomainAutostart, err := virDomain.GetAutostart()
	if err != nil {
		return util.NewError(err, "cannot get domain autostart state")
	}
	if vm.Autostart != virDomainAutostart {
		if err := virDomain.SetAutostart(vm.Autostart); err != nil {
			return util.NewError(err, "cannot set domain autostart state")
		}
	}
	return nil
}

func (repo *VirtualMachineRepository) Delete(id, nodeId string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire libvirt connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "lookup domain failed")
	}
	virDomainRunning, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot fetch domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	if virDomainRunning {
		if err := virDomain.Destroy(); err != nil {
			return util.NewError(err, "cannot destroy domain")
		}
	}
	if err := virDomain.UndefineFlags(libvirt.DOMAIN_UNDEFINE_NVRAM); err != nil {
		return util.NewError(err, "cannot undefine domain")
	}
	return nil
}

func (repo *VirtualMachineRepository) nodeList(nodeId string) ([]*compute.VirtualMachine, error) {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return nil, util.NewError(err, "cannot acquire libvirt connection")
	}
	defer repo.pool.Release(nodeId)
	vms := []*compute.VirtualMachine{}
	settings := repo.settings[nodeId]
	domains, err := conn.ListAllDomains(0)
	for _, domain := range domains {
		vm, err := repo.domainToVm(conn, nodeId, &domain, settings)
		if err != nil {
			return nil, util.NewError(err, "cannot convert libvirt domain to vm")
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func (repo *VirtualMachineRepository) List(options compute.VirtualMachineListOptions) ([]*compute.VirtualMachine, error) {
	result := []*compute.VirtualMachine{}
	nodes := repo.pool.Nodes(options.NodeIds)
	wg := &sync.WaitGroup{}
	mu := &sync.Mutex{}
	wg.Add(len(nodes))
	start := time.Now()
	for _, nodeId := range nodes {
		go func(nodeId string) {
			defer wg.Done()
			nodeStart := time.Now()
			vms, err := repo.nodeList(nodeId)
			if err != nil {
				repo.logger.Warn().Err(err).Str("node", nodeId).Msg("cannot list vms")
				return
			}
			repo.logger.Debug().Str("node", nodeId).TimeDiff("took", time.Now(), nodeStart).Msg("node vm list done")
			mu.Lock()
			result = append(result, vms...)
			mu.Unlock()
		}(nodeId)
	}
	wg.Wait()
	repo.logger.Debug().TimeDiff("took", time.Now(), start).Msg("full vm list done")
	return result, nil
}

func (repo *VirtualMachineRepository) Get(id, nodeId string) (*compute.VirtualMachine, error) {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return nil, util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	settings := repo.settings[nodeId]
	domain, err := conn.LookupDomainByName(id)
	if err != nil {
		return nil, util.NewError(err, "failed to lookup vm")
	}
	vm, err := repo.domainToVm(conn, nodeId, domain, settings)
	if err != nil {
		return nil, util.NewError(err, "cannot convert libvirt domain to vm")
	}
	return vm, nil
}

func (repo *VirtualMachineRepository) Poweroff(id, nodeId string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	domain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	return domain.Destroy()
}

func (repo *VirtualMachineRepository) Reboot(id, nodeId string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	domain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	return domain.Reboot(libvirt.DOMAIN_REBOOT_DEFAULT)
}

func (repo *VirtualMachineRepository) Start(id, nodeId string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	domain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	return domain.Create()
}

func (repo *VirtualMachineRepository) attachVolume(conn *libvirt.Connect, virDomainConfig *libvirtxml.Domain, attachedVolume *compute.VirtualMachineAttachedVolume, namer *DeviceNamer) error {
	virVolumeConfig, err := getVolumeConfigByPath(conn, attachedVolume.Path)
	if err != nil {
		return util.NewError(err, "cannot get volume config")
	}
	diskConfig := DomainDiskConfigFromVirtualMachineAttachedVolume(
		attachedVolume,
		getVolTargetFormatType(virVolumeConfig),
		virVolumeConfig.Type,
		namer,
	)
	virDomainConfig.Devices.Disks = append(virDomainConfig.Devices.Disks, *diskConfig)
	return nil
}

func (repo *VirtualMachineRepository) AttachVolume(id, nodeId string, attachedVolume *compute.VirtualMachineAttachedVolume) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	running, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	if running {
		return fmt.Errorf("domain must be stopped")
	}

	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot get domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	namer := NewDeviceNamerFromDisks(virDomainConfig.Devices.Disks)
	if err := repo.attachVolume(conn, virDomainConfig, attachedVolume, namer); err != nil {
		return util.NewError(err, "cannot add volume xml config")
	}

	virDomainXml, err = virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot create domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		fmt.Println(virDomainXml)
		return util.NewError(err, "cannot update domain")
	}
	return nil
}

func (repo *VirtualMachineRepository) DetachVolume(id, nodeId, needlePath string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	running, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	if running {
		return fmt.Errorf("domain must be stopped")
	}

	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot get domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	if virDomainConfig.Devices.Disks == nil {
		return fmt.Errorf("no disk found")
	}
	newDisks := []libvirtxml.DomainDisk{}
	needleFound := false
	for _, disk := range virDomainConfig.Devices.Disks {
		volume := VirtualMachineAttachedVolumeFromDomainDiskConfig(disk)
		if volume.Path == needlePath {
			needleFound = true
			continue
		}
		newDisks = append(newDisks, disk)
	}
	if !needleFound {
		return fmt.Errorf("no disk found")
	}
	virDomainConfig.Devices.Disks = newDisks
	virDomainXml, err = virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot create domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		return util.NewError(err, "cannot update domain")
	}
	return nil
}

func (repo *VirtualMachineRepository) AttachInterface(id, nodeId string, attachedIface *compute.VirtualMachineAttachedInterface) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	running, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	if running {
		return fmt.Errorf("domain must be stopped")
	}
	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot get domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	if err := repo.attachInterface(virDomainConfig, attachedIface); err != nil {
		return err
	}
	virDomainXml, err = virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot create domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		return util.NewError(err, "cannot update domain xml")
	}
	return nil
}

func (repo *VirtualMachineRepository) detachInterfaceLibvirt(id, nodeId, needleMac string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	running, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	if running {
		return fmt.Errorf("domain must be stopped")
	}

	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot get domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	if virDomainConfig.Devices.Interfaces == nil {
		return compute.ErrInterfaceNotFound
	}
	newInterfaces := []libvirtxml.DomainInterface{}
	needleFound := false
	for _, ifaceConfig := range virDomainConfig.Devices.Interfaces {
		iface := VirtualMachineAttachedInterfaceFromInterfaceConfig(ifaceConfig)
		if iface.Mac == needleMac {
			needleFound = true
			continue
		}
		newInterfaces = append(newInterfaces, ifaceConfig)
	}
	if !needleFound {
		return compute.ErrInterfaceNotFound
	}
	virDomainConfig.Devices.Interfaces = newInterfaces
	virDomainXml, err = virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot create domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		return util.NewError(err, "cannot update domain")
	}
	return nil
}

func (repo *VirtualMachineRepository) detachInterfaceQemuArgs(id, nodeId, needleMac string) error {
	conn, err := repo.pool.Acquire(nodeId)
	if err != nil {
		return util.NewError(err, "cannot acquire connection")
	}
	defer repo.pool.Release(nodeId)

	virDomain, err := conn.LookupDomainByName(id)
	if err != nil {
		return util.NewError(err, "domain lookup failed")
	}
	running, err := virDomain.IsActive()
	if err != nil {
		return util.NewError(err, "cannot check if domain is running")
	}
	if running {
		return fmt.Errorf("domain must be stopped")
	}
	virDomainXml, err := virDomain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return util.NewError(err, "cannot get domain xml")
	}
	virDomainConfig := &libvirtxml.Domain{}
	if err := virDomainConfig.Unmarshal(virDomainXml); err != nil {
		return util.NewError(err, "cannot parse domain xml")
	}
	if virDomainConfig.QEMUCommandline == nil {
		return compute.ErrInterfaceNotFound
	}
	if len(virDomainConfig.QEMUCommandline.Args) <= 0 {
		return compute.ErrInterfaceNotFound
	}

	needleNetdevId := ""
	for idx, qarg := range virDomainConfig.QEMUCommandline.Args {
		switch qarg.Value {
		case "-device":
			argv := virDomainConfig.QEMUCommandline.Args[idx+1].Value
			params := parseQemuDeviceArg(argv)
			fmt.Println(params)
			if strings.Contains(strings.ToLower(argv), "="+strings.ToLower(needleMac)) {
				params := parseQemuDeviceArg(argv)
				needleNetdevId = params["netdev"]
			}
		}
	}

	newArgs := []libvirtxml.DomainQEMUCommandlineArg{}
	for idx := 0; idx < len(virDomainConfig.QEMUCommandline.Args); idx++ {
		arg := virDomainConfig.QEMUCommandline.Args[idx]
		switch arg.Value {
		default:
			newArgs = append(newArgs, arg)
		case "-netdev":
			idx++
			nextarg := virDomainConfig.QEMUCommandline.Args[idx]
			params := parseQemuDeviceArg(nextarg.Value)
			if params["id"] == needleNetdevId {
				continue
			}
			newArgs = append(newArgs, arg, nextarg)
		case "-device":
			idx++
			nextarg := virDomainConfig.QEMUCommandline.Args[idx]
			params := parseQemuDeviceArg(nextarg.Value)
			if params["netdev"] == needleNetdevId {
				continue
			}
			newArgs = append(newArgs, arg, nextarg)
		}
	}
	if len(newArgs) == len(virDomainConfig.QEMUCommandline.Args) {
		return compute.ErrInterfaceNotFound
	}
	virDomainConfig.QEMUCommandline.Args = newArgs
	virDomainXml, err = virDomainConfig.Marshal()
	if err != nil {
		return util.NewError(err, "cannot create domain xml")
	}
	if _, err := conn.DomainDefineXML(virDomainXml); err != nil {
		return util.NewError(err, "cannot update domain")
	}
	return nil
}

func (repo *VirtualMachineRepository) DetachInterface(id, nodeId, needleMac string) error {
	if err := repo.detachInterfaceLibvirt(id, nodeId, needleMac); err != nil {
		if err == compute.ErrInterfaceNotFound {
			return repo.detachInterfaceQemuArgs(id, nodeId, needleMac)
		}
	}
	return nil
}
