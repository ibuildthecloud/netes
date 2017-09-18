package buse

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"

	"go.pedge.io/dlog"

	"github.com/libopenstorage/openstorage/api"
	"github.com/libopenstorage/openstorage/cluster"
	"github.com/libopenstorage/openstorage/volume"
	"github.com/libopenstorage/openstorage/volume/drivers/common"
	"github.com/pborman/uuid"
	"github.com/portworx/kvdb"
)

const (
	Name          = "buse"
	Type          = api.DriverType_DRIVER_TYPE_BLOCK
	BuseDBKey     = "OpenStorageBuseKey"
	BuseMountPath = "/var/lib/openstorage/buse/"
)

// Implements the open storage volume interface.
type driver struct {
	volume.IODriver
	volume.StoreEnumerator
	buseDevices map[string]*buseDev
}

// Implements the Device interface.
type buseDev struct {
	file string
	f    *os.File
	nbd  *NBD
}

func (d *buseDev) ReadAt(b []byte, off int64) (n int, err error) {
	return d.f.ReadAt(b, off)
}

func (d *buseDev) WriteAt(b []byte, off int64) (n int, err error) {
	return d.f.WriteAt(b, off)
}

func copyFile(source string, dest string) (err error) {
	sourcefile, err := os.Open(source)
	if err != nil {
		return err
	}

	defer sourcefile.Close()

	destfile, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer destfile.Close()

	_, err = io.Copy(destfile, sourcefile)
	if err == nil {
		sourceinfo, err := os.Stat(source)
		if err != nil {
			err = os.Chmod(dest, sourceinfo.Mode())
		}

	}

	return
}

func Init(params map[string]string) (volume.VolumeDriver, error) {
	inst := &driver{
		IODriver:        volume.IONotSupported,
		StoreEnumerator: common.NewDefaultStoreEnumerator(Name, kvdb.Instance()),
	}
	inst.buseDevices = make(map[string]*buseDev)
	if err := os.MkdirAll(BuseMountPath, 0744); err != nil {
		return nil, err
	}
	volumeInfo, err := inst.StoreEnumerator.Enumerate(
		&api.VolumeLocator{},
		nil,
	)
	if err == nil {
		for _, info := range volumeInfo {
			if info.Status == api.VolumeStatus_VOLUME_STATUS_NONE {
				info.Status = api.VolumeStatus_VOLUME_STATUS_UP
				inst.UpdateVol(info)
			}
		}
	} else {
		dlog.Println("Could not enumerate Volumes, ", err)
	}

	c, err := cluster.Inst()
	if err != nil {
		dlog.Println("BUSE initializing in single node mode")
	} else {
		dlog.Println("BUSE initializing in clustered mode")
		c.AddEventListener(inst)
	}

	dlog.Println("BUSE initialized and driver mounted at: ", BuseMountPath)
	return inst, nil
}

//
// These functions below implement the volume driver interface.
//

func (d *driver) String() string {
	return Name
}

func (d *driver) Name() string {
	return Name
}

func (d *driver) Type() api.DriverType {
	return Type
}

// Status diagnostic information
func (d *driver) Status() [][2]string {
	return [][2]string{}
}

func (d *driver) ListenerStatus() api.Status {
	return api.Status_STATUS_NONE
}

func (d *driver) ListenerData() map[string]interface{} {
	return nil
}

func (d *driver) ListenerPeerStatus() map[string]api.Status {
	return nil
}

func (d *driver) Create(locator *api.VolumeLocator, source *api.Source, spec *api.VolumeSpec) (string, error) {
	volumeID := uuid.New()
	volumeID = strings.TrimSuffix(volumeID, "\n")
	if spec.Size == 0 {
		return "", fmt.Errorf("Volume size cannot be zero", "buse")
	}
	if spec.Format == api.FSType_FS_TYPE_NONE {
		return "", fmt.Errorf("Missing volume format", "buse")
	}
	// Create a file on the local buse path with this UUID.
	buseFile := path.Join(BuseMountPath, volumeID)
	f, err := os.Create(buseFile)
	if err != nil {
		dlog.Println(err)
		return "", err
	}

	if err := f.Truncate(int64(spec.Size)); err != nil {
		dlog.Println(err)
		return "", err
	}

	bd := &buseDev{
		file: buseFile,
		f:    f,
	}
	nbd := Create(bd, int64(spec.Size))
	bd.nbd = nbd

	dlog.Infof("Connecting to NBD...")
	dev, err := bd.nbd.Connect()
	if err != nil {
		dlog.Println(err)
		return "", err
	}

	dlog.Infof("Formatting %s with %v", dev, spec.Format)
	cmd := "/sbin/mkfs." + spec.Format.SimpleString()
	o, err := exec.Command(cmd, dev).Output()
	if err != nil {
		dlog.Warnf("Failed to run command %v %v: %v", cmd, dev, o)
		return "", err
	}

	dlog.Infof("BUSE mapped NBD device %s (size=%v) to block file %s", dev, spec.Size, buseFile)

	v := common.NewVolume(
		volumeID,
		spec.Format,
		locator,
		source,
		spec,
	)
	v.DevicePath = dev

	d.buseDevices[dev] = bd

	err = d.CreateVol(v)
	if err != nil {
		return "", err
	}
	return v.Id, err
}

func (d *driver) Delete(volumeID string) error {
	v, err := d.GetVol(volumeID)
	if err != nil {
		dlog.Println(err)
		return err
	}

	bd, ok := d.buseDevices[v.DevicePath]
	if !ok {
		err = fmt.Errorf("Cannot locate a BUSE device for %s", v.DevicePath)
		dlog.Println(err)
		return err
	}

	// Clean up buse block file and close the NBD connection.
	os.Remove(bd.file)
	bd.f.Close()
	bd.nbd.Disconnect()

	dlog.Infof("BUSE deleted volume %v at NBD device %s", volumeID, v.DevicePath)

	if err := d.DeleteVol(volumeID); err != nil {
		dlog.Println(err)
		return err
	}

	return nil
}

func (d *driver) MountedAt(mountpath string) string {
	return ""
}

func (d *driver) Mount(volumeID string, mountpath string) error {
	v, err := d.GetVol(volumeID)
	if err != nil {
		return fmt.Errorf("Failed to locate volume %q", volumeID)
	}
	if len(v.AttachPath) > 0 && len(v.AttachPath) > 0 {
		return fmt.Errorf("Volume %q already mounted at %q", v.AttachPath[0])
	}
	if err := syscall.Mount(v.DevicePath, mountpath, v.Spec.Format.SimpleString(), 0, ""); err != nil {
		return fmt.Errorf("Failed to mount %v at %v: %v", v.DevicePath, mountpath, err)
	}

	dlog.Infof("BUSE mounted NBD device %s at %s", v.DevicePath, mountpath)

	if v.AttachPath == nil {
		v.AttachPath = make([]string, 1)
	}
	v.AttachPath[0] = mountpath
	return d.UpdateVol(v)
}

func (d *driver) Unmount(volumeID string, mountpath string) error {
	v, err := d.GetVol(volumeID)
	if err != nil {
		return err
	}
	if len(v.AttachPath) == 0 || len(v.AttachPath[0]) == 0 {
		return fmt.Errorf("Device %v not mounted", volumeID)
	}
	if err := syscall.Unmount(v.AttachPath[0], 0); err != nil {
		return err
	}
	v.AttachPath = nil
	return d.UpdateVol(v)
}

func (d *driver) Snapshot(volumeID string, readonly bool, locator *api.VolumeLocator) (string, error) {
	volIDs := make([]string, 1)
	volIDs[0] = volumeID
	vols, err := d.Inspect(volIDs)
	if err != nil {
		return "", nil
	}

	source := &api.Source{Parent: volumeID}
	newVolumeID, err := d.Create(locator, source, vols[0].Spec)
	if err != nil {
		return "", nil
	}

	// BUSE does not support snapshots, so just copy the block files.
	err = copyFile(BuseMountPath+volumeID, BuseMountPath+newVolumeID)
	if err != nil {
		d.Delete(newVolumeID)
		return "", nil
	}

	return newVolumeID, nil
}

func (d *driver) Set(volumeID string, locator *api.VolumeLocator, spec *api.VolumeSpec) error {
	if spec != nil {
		return volume.ErrNotSupported
	}
	v, err := d.GetVol(volumeID)
	if err != nil {
		return err
	}
	if locator != nil {
		v.Locator = locator
	}
	return d.UpdateVol(v)
}

func (d *driver) Attach(volumeID string) (string, error) {
	// Nothing to do on attach.
	return path.Join(BuseMountPath, volumeID), nil
}

func (d *driver) Detach(volumeID string) error {
	// Nothing to do on detach.
	return nil
}

func (d *driver) Stats(volumeID string, cumulative bool) (*api.Stats, error) {
	return nil, volume.ErrNotSupported
}

func (d *driver) Alerts(volumeID string) (*api.Alerts, error) {
	return nil, volume.ErrNotSupported
}

func (d *driver) Shutdown() {
	dlog.Printf("%s Shutting down", Name)
	syscall.Unmount(BuseMountPath, 0)
}

func (d *driver) ClusterInit(self *api.Node) error {
	return nil
}

func (d *driver) Init(self *api.Node, clusterInfo *cluster.ClusterInfo) error {
	return nil
}

func (d *driver) CleanupInit(self *api.Node, db *cluster.ClusterInfo) error {
	return nil
}

func (d *driver) Join(self *api.Node, initState *cluster.ClusterInitState, handleNotifications cluster.ClusterNotify) error {
	return nil
}

func (d *driver) Add(self *api.Node) error {
	return nil
}

func (d *driver) Remove(self *api.Node) error {
	return nil
}

func (d *driver) CanNodeRemove(self *api.Node) error {
	return nil
}

func (d *driver) Update(self *api.Node) error {
	return nil
}

func (d *driver) Leave(self *api.Node) error {
	return nil
}

func (d *driver) Halt(self *api.Node, db *cluster.ClusterInfo) error {
	return nil
}

func (d *driver) GetActiveRequests() (*api.ActiveRequests, error) {
	return nil, nil
}
