package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"

	"github.com/libopenstorage/openstorage/api"
	"github.com/libopenstorage/openstorage/api/spec"
	"github.com/libopenstorage/openstorage/config"
	"github.com/libopenstorage/openstorage/volume"
	"github.com/libopenstorage/openstorage/volume/drivers"
)

const (
	// VolumeDriver is the string returned in the handshake protocol.
	VolumeDriver = "VolumeDriver"
)

// Implementation of the Docker volumes plugin specification.
type driver struct {
	restBase
	spec.SpecHandler
}

type handshakeResp struct {
	Implements []string
}

type volumeRequest struct {
	Name string
	Opts map[string]string
}

type mountRequest struct {
	Name string
	ID   string
}

type volumeResponse struct {
	Err string
}

type volumePathResponse struct {
	Mountpoint string
	volumeResponse
}

type volumeInfo struct {
	Name       string
	Mountpoint string
}

type capabilities struct {
	Scope string
}

type capabilitiesResponse struct {
	Capabilities capabilities
}

func newVolumePlugin(name string) restServer {
	return &driver{restBase{name: name, version: "0.3"}, spec.NewSpecHandler()}
}

func (d *driver) String() string {
	return d.name
}

func volDriverPath(method string) string {
	return fmt.Sprintf("/%s.%s", VolumeDriver, method)
}

func (d *driver) volNotFound(request string, id string, e error, w http.ResponseWriter) error {
	err := fmt.Errorf("Failed to locate volume: " + e.Error())
	d.logRequest(request, id).Warnln(http.StatusNotFound, " ", err.Error())
	return err
}

func (d *driver) volNotMounted(request string, id string) error {
	err := fmt.Errorf("volume not mounted")
	d.logRequest(request, id).Debugln(http.StatusNotFound, " ", err.Error())
	return err
}

func (d *driver) Routes() []*Route {
	return []*Route{
		&Route{verb: "POST", path: volDriverPath("Create"), fn: d.create},
		&Route{verb: "POST", path: volDriverPath("Remove"), fn: d.remove},
		&Route{verb: "POST", path: volDriverPath("Mount"), fn: d.mount},
		&Route{verb: "POST", path: volDriverPath("Path"), fn: d.path},
		&Route{verb: "POST", path: volDriverPath("List"), fn: d.list},
		&Route{verb: "POST", path: volDriverPath("Get"), fn: d.get},
		&Route{verb: "POST", path: volDriverPath("Unmount"), fn: d.unmount},
		&Route{verb: "POST", path: volDriverPath("Capabilities"), fn: d.capabilities},
		&Route{verb: "POST", path: "/Plugin.Activate", fn: d.handshake},
		&Route{verb: "GET", path: "/status", fn: d.status},
	}
}

func (d *driver) emptyResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) errorResponse(w http.ResponseWriter, err error) {
	json.NewEncoder(w).Encode(&volumeResponse{Err: err.Error()})
}

func (d *driver) volFromName(name string) (*api.Volume, error) {
	v, err := volumedrivers.Get(d.name)
	if err != nil {
		return nil, fmt.Errorf("Cannot locate volume driver for %s: %s", d.name, err.Error())
	}
	vols, err := v.Inspect([]string{name})
	if err == nil && len(vols) == 1 {
		return vols[0], nil
	}
	vols, err = v.Enumerate(&api.VolumeLocator{Name: name}, nil)
	if err == nil && len(vols) == 1 {
		return vols[0], nil
	}
	return nil, fmt.Errorf("Cannot locate volume %s", name)
}

func (d *driver) decode(method string, w http.ResponseWriter, r *http.Request) (*volumeRequest, error) {
	var request volumeRequest
	err := json.NewDecoder(r.Body).Decode(&request)
	if err != nil {
		e := fmt.Errorf("Unable to decode JSON payload")
		d.sendError(method, "", w, e.Error()+":"+err.Error(), http.StatusBadRequest)
		return nil, e
	}
	d.logRequest(method, request.Name).Debugln("")
	return &request, nil
}

func (d *driver) decodeMount(method string, w http.ResponseWriter, r *http.Request) (*mountRequest, error) {
	var request mountRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		e := fmt.Errorf("Unable to decode JSON payload")
		d.sendError(method, "", w, e.Error()+":"+err.Error(), http.StatusBadRequest)
		return nil, e
	}
	d.logRequest(method, request.Name).Debugf("ID: %v", request.ID)
	return &request, nil
}

func (d *driver) handshake(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&handshakeResp{
		[]string{VolumeDriver},
	})
	if err != nil {
		d.sendError("handshake", "", w, "encode error", http.StatusInternalServerError)
		return
	}
	d.logRequest("handshake", "").Debugln("Handshake completed")
}

func (d *driver) status(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintln("osd plugin", d.version))
}

func (d *driver) mountpath(name string) string {
	return path.Join(volume.MountBase, name)
}

func (d *driver) create(w http.ResponseWriter, r *http.Request) {
	method := "create"
	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	specParsed, spec, name := d.SpecFromString(request.Name)
	d.logRequest(method, name).Infoln("")
	// If we fail to find the volume, create it.
	if _, err = d.volFromName(name); err != nil {
		v, err := volumedrivers.Get(d.name)
		if err != nil {
			d.errorResponse(w, err)
			return
		}

		if !specParsed {
			spec, err = d.SpecFromOpts(request.Opts)
			if err != nil {
				d.errorResponse(w, err)
				return
			}
		}

		if _, err := v.Create(
			&api.VolumeLocator{Name: name},
			nil,
			spec,
		); err != nil {
			d.errorResponse(w, err)
			return
		}
	}
	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) remove(w http.ResponseWriter, r *http.Request) {
	method := "remove"
	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf("Cannot locate volume driver")
		d.errorResponse(w, err)
		return
	}
	_, _, name := d.SpecFromString(request.Name)
	if err = v.Delete(name); err != nil {
		d.errorResponse(w, err)
		return
	}
	json.NewEncoder(w).Encode(&volumeResponse{})
}

func (d *driver) scaleUp(
	method string,
	vd volume.VolumeDriver,
	inVol *api.Volume,
) (
	outVol *api.Volume,
	err error,
) {
	i := uint32(1)
	for ; i < inVol.Spec.Scale; i++ {
		name := fmt.Sprintf("%s_%d", inVol.Locator.Name, i)
		outVol, err = d.volFromName(name)
		// If we fail to locate the volume, create it.
		if err != nil {
			id := ""
			if id, err = vd.Create(
				&api.VolumeLocator{Name: name},
				nil,
				inVol.Spec,
			); err != nil {
				return inVol, err
			}
			if outVol, err = d.volFromName(id); err != nil {
				return inVol, err
			}
		}
		// If we fail to attach the volume, continue to look for a
		// free volume.
		_, err = vd.Attach(outVol.Id)
		if err == nil {
			return outVol, nil
		}
	}
	return inVol, volume.ErrVolAttachedScale
}

func (d *driver) attachVol(
	method string,
	vd volume.VolumeDriver,
	vol *api.Volume,
) (
	outVolume *api.Volume,
	err error,
) {
	attachPath, err := vd.Attach(vol.Id)

	switch err {
	case nil:
		d.logRequest(method, vol.Locator.Name).Debugf(
			"response %v", attachPath)
		return vol, nil
	case volume.ErrVolAttachedOnRemoteNode:
		d.logRequest(method, vol.Locator.Name).Infof(
			"Mount volume attached on remote node.")
		return vol, nil
	case volume.ErrVolAttachedScale:
		d.logRequest(method, vol.Locator.Name).Infof(
			"Attempt to Scale attached volume")
		if vol.Spec.Scale > 1 {
			return d.scaleUp(method, vd, vol)
		}
		return vol, err
	default:
		d.logRequest(method, vol.Locator.Name).Warnf(
			"Cannot attach volume: %v", err.Error())
		return vol, err
	}
}

func (d *driver) mount(w http.ResponseWriter, r *http.Request) {
	var response volumePathResponse
	method := "mount"

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf("Cannot locate volume driver")
		d.errorResponse(w, err)
		return
	}

	request, err := d.decodeMount(method, w, r)
	if err != nil {
		d.errorResponse(w, err)
		return
	}
	_, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromName(name)
	if err != nil {
		d.errorResponse(w, err)
		return
	}

	// If this is a block driver, first attach the volume.
	if v.Type() == api.DriverType_DRIVER_TYPE_BLOCK {
		// If volume is scaled up, a new volume is created and
		// vol will change.
		if vol, err = d.attachVol(method, v, vol); err != nil {
			d.errorResponse(w, err)
			return
		}
	}

	// Note that name is unchanged even if a new volume was created as a
	// result of scale up.
	response.Mountpoint = d.mountpath(name)
	os.MkdirAll(response.Mountpoint, 0755)

	err = v.Mount(vol.Id, response.Mountpoint)
	if err != nil {
		d.logRequest(method, request.Name).Warnf(
			"Cannot mount volume %v, %v",
			response.Mountpoint, err)
		d.errorResponse(w, err)
		return
	}

	d.logRequest(method, request.Name).Infof("response %v", response.Mountpoint)
	json.NewEncoder(w).Encode(&response)
}

func (d *driver) path(w http.ResponseWriter, r *http.Request) {
	method := "path"
	var response volumePathResponse

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}

	_, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromName(name)
	if err != nil {
		e := d.volNotFound(method, request.Name, err, w)
		d.errorResponse(w, e)
		return
	}

	d.logRequest(method, name).Debugf("")
	if len(vol.AttachPath) == 0 || len(vol.AttachPath) == 0 {
		e := d.volNotMounted(method, name)
		d.errorResponse(w, e)
		return
	}
	response.Mountpoint = vol.AttachPath[0]
	response.Mountpoint = path.Join(response.Mountpoint, config.DataDir)
	d.logRequest(method, request.Name).Debugf("response %v", response.Mountpoint)
	json.NewEncoder(w).Encode(&response)
}

func (d *driver) list(w http.ResponseWriter, r *http.Request) {
	method := "list"

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf("Cannot locate volume driver: %v", err.Error())
		d.errorResponse(w, err)
		return
	}

	vols, err := v.Enumerate(nil, nil)
	if err != nil {
		d.errorResponse(w, err)
		return
	}

	volInfo := make([]volumeInfo, len(vols))
	for i, v := range vols {
		volInfo[i].Name = v.Locator.Name
		if len(v.AttachPath) > 0 || len(v.AttachPath) > 0 {
			volInfo[i].Mountpoint = path.Join(v.AttachPath[0], config.DataDir)
		}
	}
	json.NewEncoder(w).Encode(map[string][]volumeInfo{"Volumes": volInfo})
}

func (d *driver) get(w http.ResponseWriter, r *http.Request) {
	method := "get"

	request, err := d.decode(method, w, r)
	if err != nil {
		return
	}
	_, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromName(name)
	if err != nil {
		e := d.volNotFound(method, request.Name, err, w)
		d.errorResponse(w, e)
		return
	}

	volInfo := volumeInfo{Name: name}
	if len(vol.AttachPath) > 0 || len(vol.AttachPath) > 0 {
		volInfo.Mountpoint = path.Join(vol.AttachPath[0], config.DataDir)
	}

	json.NewEncoder(w).Encode(map[string]volumeInfo{"Volume": volInfo})
}

func (d *driver) unmount(w http.ResponseWriter, r *http.Request) {
	method := "unmount"

	v, err := volumedrivers.Get(d.name)
	if err != nil {
		d.logRequest(method, "").Warnf(
			"Cannot locate volume driver: %v",
			err.Error())
		d.errorResponse(w, err)
		return
	}

	request, err := d.decodeMount(method, w, r)
	if err != nil {
		return
	}

	_, _, name := d.SpecFromString(request.Name)
	vol, err := d.volFromName(name)
	if err != nil {
		e := d.volNotFound(method, name, err, w)
		d.errorResponse(w, e)
		return
	}

	mountpoint := d.mountpath(name)
	id := vol.Id
	if vol.Spec.Scale > 1 {
		id = v.MountedAt(mountpoint)
		if len(id) == 0 {
			err := fmt.Errorf("Failed to find volume mapping for %v",
				mountpoint)
			d.logRequest(method, request.Name).Warnf(
				"Cannot unmount volume %v, %v",
				mountpoint, err)
			d.errorResponse(w, err)
			return
		}
	}
	err = v.Unmount(id, mountpoint)
	if err != nil {
		d.logRequest(method, request.Name).Warnf(
			"Cannot unmount volume %v, %v",
			mountpoint, err)
		d.errorResponse(w, err)
		return
	}

	if v.Type() == api.DriverType_DRIVER_TYPE_BLOCK {
		_ = v.Detach(id)
	}
	d.emptyResponse(w)
}

func (d *driver) capabilities(w http.ResponseWriter, r *http.Request) {
	method := "capabilities"
	var response capabilitiesResponse

	response.Capabilities.Scope = "global"
	d.logRequest(method, "").Infof("response %v", response.Capabilities.Scope)
	json.NewEncoder(w).Encode(&response)
}
