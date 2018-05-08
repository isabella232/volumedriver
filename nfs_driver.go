package nfsdriver

import (
	"errors"
	"fmt"
	"os"
	"time"

	"path/filepath"

	"encoding/json"
	"sync"

	"context"

	"code.cloudfoundry.org/goshims/filepathshim"
	"code.cloudfoundry.org/goshims/ioutilshim"
	"code.cloudfoundry.org/goshims/osshim"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/voldriver"
	"code.cloudfoundry.org/voldriver/driverhttp"
)

type NfsVolumeInfo struct {
	Opts                 map[string]interface{} `json:"-"` // don't store opts
	wg                   sync.WaitGroup
	mountError           string
	voldriver.VolumeInfo // see voldriver.resources.go
}

type OsHelper interface {
	Umask(mask int) (oldmask int)
}

type NfsDriver struct {
	volumes       map[string]*NfsVolumeInfo
	volumesLock   sync.RWMutex
	os            osshim.Os
	filepath      filepathshim.Filepath
	ioutil        ioutilshim.Ioutil
	mountPathRoot string
	mounter       Mounter
	osHelper      OsHelper
}

func NewNfsDriver(logger lager.Logger, os osshim.Os, filepath filepathshim.Filepath, ioutil ioutilshim.Ioutil, mountPathRoot string, mounter Mounter, oshelper OsHelper) *NfsDriver {
	d := &NfsDriver{
		volumes:       map[string]*NfsVolumeInfo{},
		os:            os,
		filepath:      filepath,
		ioutil:        ioutil,
		mountPathRoot: mountPathRoot,
		mounter:       mounter,
		osHelper:      oshelper,
	}

	ctx := context.TODO()
	env := driverhttp.NewHttpDriverEnv(logger, ctx)

	d.restoreState(env)
	d.checkMounts(env)

	return d
}

func (d *NfsDriver) Activate(env voldriver.Env) voldriver.ActivateResponse {
	return voldriver.ActivateResponse{
		Implements: []string{"VolumeDriver"},
	}
}

func (d *NfsDriver) Create(env voldriver.Env, createRequest voldriver.CreateRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("create")
	logger.Info("start")
	defer logger.Info("end")

	if createRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	var ok bool
	if _, ok = createRequest.Opts["source"].(string); !ok {
		logger.Info("mount-config-missing-source", lager.Data{"volume_name": createRequest.Name})
		return voldriver.ErrorResponse{Err: `Missing mandatory 'source' field in 'Opts'`}
	}

	existing, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), createRequest.Name)

	if err != nil {
		logger.Info("creating-volume", lager.Data{"volume_name": createRequest.Name})
		logger.Info("with-opts", lager.Data{"opts": createRequest.Opts})

		volInfo := NfsVolumeInfo{
			VolumeInfo: voldriver.VolumeInfo{Name: createRequest.Name},
			Opts:       createRequest.Opts,
		}

		d.volumesLock.Lock()
		defer d.volumesLock.Unlock()

		d.volumes[createRequest.Name] = &volInfo
	} else {
		existing.Opts = createRequest.Opts

		d.volumesLock.Lock()
		defer d.volumesLock.Unlock()

		d.volumes[createRequest.Name] = existing
	}

	err = d.persistState(driverhttp.EnvWithLogger(logger, env))
	if err != nil {
		logger.Error("persist-state-failed", err)
		return voldriver.ErrorResponse{Err: fmt.Sprintf("persist state failed when creating: %s", err.Error())}
	}

	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) List(_ voldriver.Env) voldriver.ListResponse {
	d.volumesLock.RLock()
	defer d.volumesLock.RUnlock()

	listResponse := voldriver.ListResponse{
		Volumes: []voldriver.VolumeInfo{},
	}

	for _, volume := range d.volumes {
		listResponse.Volumes = append(listResponse.Volumes, volume.VolumeInfo)
	}
	listResponse.Err = ""
	return listResponse
}

func (d *NfsDriver) Mount(env voldriver.Env, mountRequest voldriver.MountRequest) voldriver.MountResponse {
	logger := env.Logger().Session("mount", lager.Data{"volume": mountRequest.Name})
	logger.Info("start")
	defer logger.Info("end")

	if mountRequest.Name == "" {
		return voldriver.MountResponse{Err: "Missing mandatory 'volume_name'"}
	}

	var doMount bool
	var opts map[string]interface{}
	var mountPath string
	var wg *sync.WaitGroup

	ret := func() voldriver.MountResponse {

		d.volumesLock.Lock()
		defer d.volumesLock.Unlock()

		volume := d.volumes[mountRequest.Name]
		if volume == nil {
			return voldriver.MountResponse{Err: fmt.Sprintf("Volume '%s' must be created before being mounted", mountRequest.Name)}
		}

		mountPath = d.mountPath(driverhttp.EnvWithLogger(logger, env), volume.Name)

		logger.Info("mounting-volume", lager.Data{"id": volume.Name, "mountpoint": mountPath})
		logger.Info("mount-source", lager.Data{"source": volume.Opts["source"].(string)})

		if volume.MountCount < 1 {
			doMount = true
			volume.wg.Add(1)
			opts = map[string]interface{}{}
			for k, v := range volume.Opts {
				opts[k] = v
			}
		}

		volume.Mountpoint = mountPath
		volume.MountCount++

		logger.Info("volume-mounted", lager.Data{"name": volume.Name, "count": volume.MountCount})

		if err := d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
			logger.Error("persist-state-failed", err)
			return voldriver.MountResponse{Err: fmt.Sprintf("persist state failed when mounting: %s", err.Error())}
		}

		wg = &volume.wg
		return voldriver.MountResponse{Mountpoint: volume.Mountpoint}
	}()

	if ret.Err != "" {
		return ret
	}

	if doMount {
		mountStartTime := time.Now()

		err := d.mount(driverhttp.EnvWithLogger(logger, env), opts, mountPath)

		mountEndTime := time.Now()
		mountDuration := mountEndTime.Sub(mountStartTime)
		if mountDuration > 8*time.Second {
			logger.Error("mount-duration-too-high", nil, lager.Data{"mount-duration-in-second": mountDuration / time.Second, "warning": "This may result in container creation failure!"})
		}

		func() {
			d.volumesLock.Lock()
			defer d.volumesLock.Unlock()

			volume := d.volumes[mountRequest.Name]
			if volume == nil {
				ret = voldriver.MountResponse{Err: fmt.Sprintf("Volume '%s' not found", mountRequest.Name)}
			} else if err != nil {
				if _, ok := err.(voldriver.SafeError); ok {
					errBytes, m_err := json.Marshal(err)
					if m_err != nil {
						logger.Error("failed-to-marshal-safeerror", m_err)
						volume.mountError = err.Error()
					}
					volume.mountError = string(errBytes)
				} else {
					volume.mountError = err.Error()
				}
			}
		}()

		wg.Done()
	}

	wg.Wait()

	return func() voldriver.MountResponse {
		d.volumesLock.Lock()
		defer d.volumesLock.Unlock()

		volume := d.volumes[mountRequest.Name]
		if volume == nil {
			return voldriver.MountResponse{Err: fmt.Sprintf("Volume '%s' not found", mountRequest.Name)}
		} else if volume.mountError != "" {
			return voldriver.MountResponse{Err: volume.mountError}
		} else {
			// Check the volume to make sure it's still mounted before handing it out again.
			if !doMount && !d.mounter.Check(driverhttp.EnvWithLogger(logger, env), volume.Name, volume.Mountpoint) {
				wg.Add(1)
				defer wg.Done()
				if err := d.mount(driverhttp.EnvWithLogger(logger, env), volume.Opts, mountPath); err != nil {
					logger.Error("remount-volume-failed", err)
					return voldriver.MountResponse{Err: fmt.Sprintf("Error remounting volume: %s", err.Error())}
				}
			}
			return voldriver.MountResponse{Mountpoint: volume.Mountpoint}
		}
	}()
}

func (d *NfsDriver) Path(env voldriver.Env, pathRequest voldriver.PathRequest) voldriver.PathResponse {
	logger := env.Logger().Session("path", lager.Data{"volume": pathRequest.Name})

	if pathRequest.Name == "" {
		return voldriver.PathResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), pathRequest.Name)
	if err != nil {
		logger.Error("failed-no-such-volume-found", err, lager.Data{"mountpoint": vol.Mountpoint})

		return voldriver.PathResponse{Err: fmt.Sprintf("Volume '%s' not found", pathRequest.Name)}
	}

	if vol.Mountpoint == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.PathResponse{Err: errText}
	}

	return voldriver.PathResponse{Mountpoint: vol.Mountpoint}
}

func (d *NfsDriver) Unmount(env voldriver.Env, unmountRequest voldriver.UnmountRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("unmount", lager.Data{"volume": unmountRequest.Name})

	if unmountRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()

	volume, ok := d.volumes[unmountRequest.Name]
	if !ok {
		logger.Error("failed-no-such-volume-found", fmt.Errorf("could not find volume %s", unmountRequest.Name))

		return voldriver.ErrorResponse{Err: fmt.Sprintf("Volume '%s' not found", unmountRequest.Name)}
	}

	if volume.Mountpoint == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.ErrorResponse{Err: errText}
	}

	if volume.MountCount == 1 {
		if err := d.unmount(driverhttp.EnvWithLogger(logger, env), unmountRequest.Name, volume.Mountpoint); err != nil {
			return voldriver.ErrorResponse{Err: err.Error()}
		}
	}

	volume.MountCount--

	if volume.MountCount < 1 {
		delete(d.volumes, unmountRequest.Name)
	}

	if err := d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
		return voldriver.ErrorResponse{Err: fmt.Sprintf("failed to persist state when unmounting: %s", err.Error())}
	}

	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) Remove(env voldriver.Env, removeRequest voldriver.RemoveRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("remove", lager.Data{"volume": removeRequest})
	logger.Info("start")
	defer logger.Info("end")

	if removeRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), removeRequest.Name)

	if err != nil {
		logger.Error("warning-volume-removal", fmt.Errorf(fmt.Sprintf("Volume %s not found", removeRequest.Name)))
		return voldriver.ErrorResponse{}
	}

	if vol.Mountpoint != "" {
		if err := d.unmount(driverhttp.EnvWithLogger(logger, env), removeRequest.Name, vol.Mountpoint); err != nil {
			return voldriver.ErrorResponse{Err: err.Error()}
		}
	}

	logger.Info("removing-volume", lager.Data{"name": removeRequest.Name})

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()
	delete(d.volumes, removeRequest.Name)

	if err := d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
		return voldriver.ErrorResponse{Err: fmt.Sprintf("failed to persist state when removing: %s", err.Error())}
	}

	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) Get(env voldriver.Env, getRequest voldriver.GetRequest) voldriver.GetResponse {
	volume, err := d.getVolume(env, getRequest.Name)
	if err != nil {
		return voldriver.GetResponse{Err: err.Error()}
	}

	return voldriver.GetResponse{
		Volume: voldriver.VolumeInfo{
			Name:       getRequest.Name,
			Mountpoint: volume.Mountpoint,
		},
	}
}

func (d *NfsDriver) getVolume(env voldriver.Env, volumeName string) (*NfsVolumeInfo, error) {
	logger := env.Logger().Session("get-volume")
	d.volumesLock.RLock()
	defer d.volumesLock.RUnlock()

	if vol, ok := d.volumes[volumeName]; ok {
		logger.Info("getting-volume", lager.Data{"name": volumeName})
		return vol, nil
	}

	return &NfsVolumeInfo{}, errors.New("Volume not found")
}

func (d *NfsDriver) Capabilities(env voldriver.Env) voldriver.CapabilitiesResponse {
	return voldriver.CapabilitiesResponse{
		Capabilities: voldriver.CapabilityInfo{Scope: "local"},
	}
}

func (d *NfsDriver) exists(path string) (bool, error) {
	_, err := d.os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func (d *NfsDriver) mountPath(env voldriver.Env, volumeId string) string {
	logger := env.Logger().Session("mount-path")
	orig := d.osHelper.Umask(000)
	defer d.osHelper.Umask(orig)

	dir, err := d.filepath.Abs(d.mountPathRoot)
	if err != nil {
		logger.Fatal("abs-failed", err)
	}

	if err := d.os.MkdirAll(dir, os.ModePerm); err != nil {
		logger.Fatal("mkdir-rootpath-failed", err)
	}

	return filepath.Join(dir, volumeId)
}

func (d *NfsDriver) mount(env voldriver.Env, opts map[string]interface{}, mountPath string) error {
	source, sourceOk := opts["source"].(string)
	logger := env.Logger().Session("mount", lager.Data{"source": source, "target": mountPath})
	logger.Info("start")
	defer logger.Info("end")

	if !sourceOk {
		err := errors.New("no source information")
		logger.Error("unable-to-extract-source", err)
		return err
	}

	orig := d.osHelper.Umask(000)
	defer d.osHelper.Umask(orig)

	err := d.os.MkdirAll(mountPath, os.ModePerm)
	if err != nil {
		logger.Error("create-mountdir-failed", err)
		return err
	}

	// TODO--permissions & flags?
	err = d.mounter.Mount(env, source, mountPath, opts)
	if err != nil {
		logger.Error("mount-failed: ", err)
		rm_err := d.os.RemoveAll(mountPath)
		if rm_err != nil {
			logger.Error("mount-removeall-failed", err, lager.Data{"mount-path": mountPath})
		}
	}
	return err
}

func (d *NfsDriver) persistState(env voldriver.Env) error {
	// TODO--why are we passing state instead of using the one in d?

	logger := env.Logger().Session("persist-state")
	logger.Info("start")
	defer logger.Info("end")

	orig := d.osHelper.Umask(000)
	defer d.osHelper.Umask(orig)

	stateFile := d.mountPath(env, "driver-state.json")

	stateData, err := json.Marshal(d.volumes)
	if err != nil {
		logger.Error("failed-to-marshall-state", err)
		return err
	}

	err = d.ioutil.WriteFile(stateFile, stateData, os.ModePerm)
	if err != nil {
		logger.Error("failed-to-write-state-file", err, lager.Data{"stateFile": stateFile})
		return err
	}

	logger.Debug("state-saved", lager.Data{"state-file": stateFile})
	return nil
}

func (d *NfsDriver) restoreState(env voldriver.Env) {
	logger := env.Logger().Session("restore-state")
	logger.Info("start")
	defer logger.Info("end")

	stateFile := filepath.Join(d.mountPathRoot, "driver-state.json")

	stateData, err := d.ioutil.ReadFile(stateFile)
	if err != nil {
		logger.Info("failed-to-read-state-file", lager.Data{"err": err, "stateFile": stateFile})
		return
	}

	state := map[string]*NfsVolumeInfo{}
	err = json.Unmarshal(stateData, &state)

	logger.Info("state", lager.Data{"state": state})

	if err != nil {
		logger.Error("failed-to-unmarshall-state", err, lager.Data{"stateFile": stateFile})
		return
	}
	logger.Info("state-restored", lager.Data{"state-file": stateFile})

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()
	d.volumes = state
}

func (d *NfsDriver) unmount(env voldriver.Env, name string, mountPath string) error {
	logger := env.Logger().Session("unmount")
	logger.Info("start")
	defer logger.Info("end")

	exists, err := d.exists(mountPath)
	if err != nil {
		logger.Error("failed-retrieving-mount-info", err, lager.Data{"mountpoint": mountPath})
	}

	if !exists {
		errText := fmt.Sprintf("Volume %s does not exist (path: %s), nothing to do!", name, mountPath)
		logger.Error("failed-mountpoint-not-found", errors.New(errText))
		return errors.New(errText)
	}

	logger.Info("unmount-volume-folder", lager.Data{"mountpath": mountPath})

	err = d.mounter.Unmount(env, mountPath)
	if err != nil {
		logger.Error("unmount-failed", err)
		return fmt.Errorf("Error unmounting volume: %s", err.Error())
	}
	err = d.os.RemoveAll(mountPath)
	if err != nil {
		logger.Error("remove-mountdir-failed", err)
		return fmt.Errorf("Error removing mountpoint: %s", err.Error())
	}

	logger.Info("unmounted-volume")

	return nil
}

func (d *NfsDriver) checkMounts(env voldriver.Env) {
	logger := env.Logger().Session("check-mounts")
	logger.Info("start")
	defer logger.Info("end")

	for key, mount := range d.volumes {
		if !d.mounter.Check(driverhttp.EnvWithLogger(logger, env), key, mount.VolumeInfo.Mountpoint) {
			delete(d.volumes, key)
		}
	}
}

func (d *NfsDriver) Drain(env voldriver.Env) error {
	logger := env.Logger().Session("check-mounts")
	logger.Info("start")
	defer logger.Info("end")

	// flush any volumes that are still in our map
	for key, mount := range d.volumes {
		if mount.Mountpoint != "" && mount.MountCount > 0 {
			err := d.unmount(env, mount.Name, mount.Mountpoint)
			if err != nil {
				logger.Error("drain-unmount-failed", err, lager.Data{"mount-name": mount.Name, "mount-point": mount.Mountpoint})
			}
		}
		delete(d.volumes, key)
	}

	d.mounter.Purge(env, d.mountPathRoot)

	return nil
}
