// Copyright (c) 2017 Huawei Technologies Co., Ltd. All Rights Reserved.
//
//    Licensed under the Apache License, Version 2.0 (the "License"); you may
//    not use this file except in compliance with the License. You may obtain
//    a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
//    WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
//    License for the specific language governing permissions and limitations
//    under the License.

package lvm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"

	log "github.com/golang/glog"
	"github.com/opensds/opensds/contrib/backup"
	"github.com/opensds/opensds/contrib/connector"
	"github.com/opensds/opensds/contrib/drivers/lvm/targets"
	. "github.com/opensds/opensds/contrib/drivers/utils/config"
	pb "github.com/opensds/opensds/pkg/dock/proto"
	"github.com/opensds/opensds/pkg/model"
	"github.com/opensds/opensds/pkg/utils"
	"github.com/opensds/opensds/pkg/utils/config"
	"github.com/satori/go.uuid"
)

const (
	defaultTgtConfDir = "/etc/tgt/conf.d"
	defaultTgtBindIp  = "127.0.0.1"
	defaultConfPath   = "/etc/opensds/driver/lvm.yaml"
	volumePrefix      = "volume-"
	snapshotPrefix    = "_snapshot-"
	blocksize         = 4096
	sizeShiftBit      = 30

	//LVPath "LV Path"
	LVPath = "LV Path"
	//LVSnapshotStatus "LV snapshot status"
	LVSnapshotStatus = "LV snapshot status"
)

type LvInfo struct {
	Name string
	Vg   string
	Size int64
}

type LVMConfig struct {
	TgtBindIp      string                    `yaml:"tgtBindIp"`
	TgtConfDir     string                    `yaml:"tgtConfDir"`
	EnableChapAuth bool                      `yaml:"enableChapAuth"`
	Pool           map[string]PoolProperties `yaml:"pool,flow"`
}

type Driver struct {
	conf *LVMConfig

	handler func(script string, cmd []string) (string, error)
}

func (d *Driver) Setup() error {
	// Read lvm config file
	d.conf = &LVMConfig{TgtBindIp: defaultTgtBindIp, TgtConfDir: defaultTgtConfDir}
	p := config.CONF.OsdsDock.Backends.LVM.ConfigPath
	if "" == p {
		p = defaultConfPath
	}
	if _, err := Parse(d.conf, p); err != nil {
		return err
	}
	d.handler = execCmd

	return nil
}

func (*Driver) Unset() error { return nil }

func (d *Driver) copySnapshotToVolume(opt *pb.CreateVolumeOpts, lvPath string) error {
	var snapSize = uint64(opt.GetSnapshotSize())
	var count = (snapSize << sizeShiftBit) / blocksize
	var snapName = snapshotPrefix + opt.GetSnapshotId()
	var snapPath = path.Join("/dev", opt.GetPoolName(), snapName)
	if _, err := d.handler("dd", []string{
		"if=" + snapPath,
		"of=" + lvPath,
		"count=" + fmt.Sprint(count),
		"bs=" + fmt.Sprint(blocksize),
	}); err != nil {
		log.Error("Failed to create logic volume:", err)
		return err
	}
	return nil
}

func (d *Driver) downloadSnapshot(bucket, backupId, dest string) error {
	mc, err := backup.NewBackup("multi-cloud")
	if err != nil {
		log.Errorf("get backup driver, err: %v", err)
		return err
	}

	if err := mc.SetUp(); err != nil {
		return err
	}
	defer mc.CleanUp()

	file, err := os.OpenFile(dest, os.O_RDWR, 0666)
	if err != nil {
		log.Errorf("open lvm snapshot file, err: %v", err)
		return err
	}
	defer file.Close()

	metadata := map[string]string{
		"bucket": bucket,
	}
	b := &backup.BackupSpec{
		Metadata: metadata,
	}

	if err := mc.Restore(b, backupId, file); err != nil {
		log.Errorf("upload snapshot to multi-cloud failed, err: %v", err)
		return err
	}
	return nil
}

func (d *Driver) CreateVolume(opt *pb.CreateVolumeOpts) (vol *model.VolumeSpec, err error) {
	var size = fmt.Sprint(opt.GetSize()) + "G"
	var polName = opt.GetPoolName()
	var id = opt.GetId()
	var name = volumePrefix + id

	if _, err := d.handler("lvcreate", []string{
		"-Z", "n",
		"-n", name, // use uuid instead of name.
		"-L", size,
		polName,
	}); err != nil {
		log.Error("Failed to create logic volume:", err)
		return nil, err
	}

	var lvPath, lvStatus string
	// Display and parse some metadata in logic volume returned.
	lvPath = path.Join("/dev", polName, name)
	lv, err := d.handler("lvdisplay", []string{lvPath})
	if err != nil {
		log.Error("Failed to display logic volume:", err)
		return nil, err
	}

	for _, line := range strings.Split(lv, "\n") {
		if strings.Contains(line, "LV Path") {
			lvPath = strings.Fields(line)[2]
		}
		if strings.Contains(line, "LV Status") {
			lvStatus = strings.Fields(line)[2]
		}
	}

	// remove created volume if got error
	defer func() {
		// using return value as the error flag
		if vol == nil {
			_, err := d.handler("lvremove", []string{"-f", lvPath})
			if err != nil {
				log.Error("Failed to remove logic volume:", err)
			}
		}
	}()

	// Create volume from snapshot
	if opt.GetSnapshotId() != "" {
		if opt.SnapshotFromCloud {
			// download cloud snapshot to volume
			data := opt.GetMetadata()
			backupId, ok := data["backupId"]
			if !ok {
				return nil, errors.New("can't find backupId in metadata")
			}
			bucket, ok := data["bucket"]
			if !ok {
				return nil, errors.New("can't find bucket name in metadata")
			}
			err := d.downloadSnapshot(bucket, backupId, lvPath)
			if err != nil {
				log.Errorf("Download snapshot failed, %v", err)
				return nil, err
			}
		} else {
			// copy local snapshot to volume
			if err := d.copySnapshotToVolume(opt, lvPath); err != nil {
				log.Errorf("Copy snapshot to volume failed, %v", err)
				return nil, err
			}
		}
	}

	return &model.VolumeSpec{
		BaseModel: &model.BaseModel{
			Id: opt.GetId(),
		},
		Name:        opt.GetName(),
		Size:        opt.GetSize(),
		Description: opt.GetDescription(),
		Status:      lvStatus,
		Metadata: map[string]string{
			"lvPath": lvPath,
		},
	}, nil
}

func (d *Driver) PullVolume(volIdentifier string) (*model.VolumeSpec, error) {
	// Display and parse some metadata in logic volume returned.
	lv, err := d.handler("lvdisplay", []string{volIdentifier})
	if err != nil {
		log.Error("Failed to display logic volume:", err)
		return nil, err
	}
	var lvStatus string
	for _, line := range strings.Split(lv, "\n") {
		if strings.Contains(line, "LV Status") {
			lvStatus = strings.Fields(line)[2]
		}
	}

	return &model.VolumeSpec{
		Status: lvStatus,
	}, nil
}

func (d *Driver) geLvInfos() ([]*LvInfo, error) {
	var lvList []*LvInfo
	args := []string{"--noheadings", "--unit=g", "-o", "vg_name,name,size", "--nosuffix"}
	info, err := d.handler("lvs", args)
	if err != nil {
		log.Error("Get volume failed", err)
		return lvList, err
	}
	for _, line := range strings.Split(info, "\n") {
		if len(line) == 0 {
			continue
		}
		words := strings.Fields(line)
		size, _ := strconv.ParseInt(words[2], 10, 64)
		lv := &LvInfo{
			Vg:   words[0],
			Name: words[1],
			Size: size,
		}
		lvList = append(lvList, lv)
	}
	return lvList, nil
}

func (d *Driver) volumeExists(id string) bool {
	lvList, _ := d.geLvInfos()
	name := volumePrefix + id
	for _, lv := range lvList {
		if lv.Name == name {
			return true
		}
	}
	return false
}

func (d *Driver) lvHasSnapshot(lvPath string) bool {
	args := []string{"--noheading", "-C", "-o", "Attr", lvPath}
	info, err := d.handler("lvdisplay", args)
	if err != nil {
		log.Error("Failed to display logic volume:", err)
		return false
	}
	info = strings.Trim(info, " ")
	return info[0] == 'o' || info[0] == 'O'
}

func (d *Driver) getActiveSnapshotsPathsOfLv(lvPath string) ([]string, error) {
	array := strings.Split(lvPath, "/")
	lvName := array[len(array)-1]
	var snapshotsPaths []string

	args := []string{}
	info, err := d.handler("lvdisplay", args)
	if err != nil {
		log.Error("Failed to display logic volume:", err)
		return snapshotsPaths, err
	}

	lvInfoList := strings.Split(info, "--- Logical volume ---")
	for _, lvInfo := range lvInfoList {
		lines := strings.Split(lvInfo, "\n")
		var path string

		for _, line := range lines {
			line = strings.Trim(line, " ")

			if strings.HasPrefix(line, LVPath) {
				path = strings.Split(line, LVPath)[1]
				path = strings.Trim(path, " ")
			}

			if strings.HasPrefix(line, LVSnapshotStatus) {
				snapshotStatus := strings.Split(line, LVSnapshotStatus)[1]
				snapshotStatus = strings.Trim(snapshotStatus, " ")

				if ("active destination for " + lvName) == snapshotStatus {
					snapshotsPaths = append(snapshotsPaths, path)
				}
			}
		}
	}

	return snapshotsPaths, nil
}

func (d *Driver) deactivateSnapshotsOfLv(snapshotsPaths []string) error {
	for _, snapshotPath := range snapshotsPaths {
		if _, err := d.handler("lvchange", []string{
			"-an", "-y", snapshotPath,
		}); err != nil {
			log.Error("Failed to deactivate snapshot:", err)
			return err
		}
	}

	return nil
}

func (d *Driver) DeleteVolume(opt *pb.DeleteVolumeOpts) error {

	id := opt.GetId()
	if !d.volumeExists(id) {
		log.Warningf("Volume(%s) does not exist, nothing to remove", id)
		return nil
	}

	lvPath, ok := opt.GetMetadata()["lvPath"]
	if !ok {
		err := errors.New("failed to find logic volume path in volume metadata")
		log.Error(err)
		return err
	}

	if d.lvHasSnapshot(lvPath) {
		err := fmt.Errorf("unable to delete due to existing snapshot for volume: %s", id)
		log.Error(err)
		return err
	}

	if _, err := d.handler("lvremove", []string{"-f", lvPath}); err != nil {
		log.Error("Failed to remove logic volume:", err)
		return err
	}

	return nil
}

// ExtendVolume ...
func (d *Driver) ExtendVolume(opt *pb.ExtendVolumeOpts) (*model.VolumeSpec, error) {
	lvPath, ok := opt.GetMetadata()["lvPath"]
	if !ok {
		err := errors.New("failed to find logic volume path in volume metadata")
		log.Error(err)
		return nil, err
	}

	if d.lvHasSnapshot(lvPath) {
		snapshotsPathsOfLv, err := d.getActiveSnapshotsPathsOfLv(lvPath)
		if err != nil {
			log.Error(err)
			return nil, err
		}

		if len(snapshotsPathsOfLv) > 0 {
			err = d.deactivateSnapshotsOfLv(snapshotsPathsOfLv)
			if err != nil {
				log.Error(err)
				return nil, err
			}
		}
	}

	var size = fmt.Sprint(opt.GetSize()) + "G"

	if _, err := d.handler("lvresize", []string{
		"-L", size,
		lvPath,
	}); err != nil {
		log.Error("Failed to extend logic volume:", err)
		return nil, err
	}

	return &model.VolumeSpec{
		BaseModel: &model.BaseModel{
			Id: opt.GetId(),
		},
		Name:        opt.GetName(),
		Size:        opt.GetSize(),
		Description: opt.GetDescription(),
		Metadata:    opt.GetMetadata(),
	}, nil
}

func (d *Driver) InitializeConnection(opt *pb.CreateAttachmentOpts) (*model.ConnectionInfo, error) {
	initiator := opt.HostInfo.GetInitiator()
	if initiator == "" {
		initiator = "ALL"
	}

	hostIP := opt.HostInfo.GetIp()
	if hostIP == "" {
		hostIP = "ALL"
	}

	lvPath, ok := opt.GetMetadata()["lvPath"]
	if !ok {
		err := errors.New("Failed to find logic volume path in volume attachment metadata!")
		log.Error(err)
		return nil, err
	}
	var chapAuth []string
	if d.conf.EnableChapAuth {
		chapAuth = []string{utils.RandSeqWithAlnum(20), utils.RandSeqWithAlnum(16)}
	}
	t := targets.NewTarget(d.conf.TgtBindIp, d.conf.TgtConfDir)
	expt, err := t.CreateExport(opt.GetVolumeId(), lvPath, hostIP, initiator, chapAuth)
	if err != nil {
		log.Error("Failed to initialize connection of logic volume:", err)
		return nil, err
	}

	return &model.ConnectionInfo{
		DriverVolumeType: ISCSIProtocol,
		ConnectionData:   expt,
	}, nil
}

func (d *Driver) TerminateConnection(opt *pb.DeleteAttachmentOpts) error {
	t := targets.NewTarget(d.conf.TgtBindIp, d.conf.TgtConfDir)
	if err := t.RemoveExport(opt.GetVolumeId()); err != nil {
		log.Error("Failed to initialize connection of logic volume:", err)
		return err
	}
	return nil
}

func (d *Driver) AttachSnapshot(snapshotId string, lvsPath string) (string, *model.ConnectionInfo, error) {

	var err error
	createOpt := &pb.CreateSnapshotAttachmentOpts{
		SnapshotId: snapshotId,
		Metadata: map[string]string{
			"lvsPath": lvsPath,
		},
		HostInfo: &pb.HostInfo{
			Platform:  runtime.GOARCH,
			OsType:    runtime.GOOS,
			Host:      d.conf.TgtBindIp,
			Initiator: "",
		},
	}

	info, err := d.InitializeSnapshotConnection(createOpt)
	if err != nil {
		return "", nil, err
	}

	// rollback
	defer func() {
		if err != nil {
			deleteOpt := &pb.DeleteSnapshotAttachmentOpts{}
			d.TerminateSnapshotConnection(deleteOpt)
		}
	}()

	conn := connector.NewConnector(info.DriverVolumeType)
	mountPoint, err := conn.Attach(info.ConnectionData)
	if err != nil {
		return "", nil, err
	}
	log.Infof("Attach snapshot success, MountPoint:%s", mountPoint)
	return mountPoint, info, nil
}

func (d *Driver) DetachSnapshot(snapshotId string, info *model.ConnectionInfo) error {

	con := connector.NewConnector(info.DriverVolumeType)
	if con == nil {
		return fmt.Errorf("Can not find connector (%s)!", info.DriverVolumeType)
	}

	con.Detach(info.ConnectionData)
	attach := &pb.DeleteSnapshotAttachmentOpts{
		SnapshotId:     snapshotId,
		AccessProtocol: info.DriverVolumeType,
	}
	return d.TerminateSnapshotConnection(attach)
}

func (d *Driver) uploadSnapshot(lvsPath string, bucket string) (string, error) {
	mc, err := backup.NewBackup("multi-cloud")
	if err != nil {
		log.Errorf("get backup driver, err: %v", err)
		return "", err
	}

	if err := mc.SetUp(); err != nil {
		return "", err
	}
	defer mc.CleanUp()

	file, err := os.Open(lvsPath)
	if err != nil {
		log.Errorf("open lvm snapshot file, err: %v", err)
		return "", err
	}
	defer file.Close()

	metadata := map[string]string{
		"bucket": bucket,
	}
	b := &backup.BackupSpec{
		Id:       uuid.NewV4().String(),
		Metadata: metadata,
	}

	if err := mc.Backup(b, file); err != nil {
		log.Errorf("upload snapshot to multi-cloud failed, err: %v", err)
		return "", err
	}
	return b.Id, nil
}

func (d *Driver) deleteUploadedSnapshot(backupId string, bucket string) error {
	mc, err := backup.NewBackup("multi-cloud")
	if err != nil {
		log.Errorf("get backup driver failed, err: %v", err)
		return err
	}

	if err := mc.SetUp(); err != nil {
		return err
	}
	defer mc.CleanUp()

	metadata := map[string]string{
		"bucket": bucket,
	}
	b := &backup.BackupSpec{
		Id:       backupId,
		Metadata: metadata,
	}

	if err := mc.Delete(b); err != nil {
		log.Errorf("delete backup snapshot  failed, err: %v", err)
		return err
	}
	return nil
}

func (d *Driver) CreateSnapshot(opt *pb.CreateVolumeSnapshotOpts) (snap *model.VolumeSnapshotSpec, err error) {
	var size = fmt.Sprint(opt.GetSize()) + "G"
	var id = opt.GetId()
	var snapName = snapshotPrefix + id

	lvPath, ok := opt.GetMetadata()["lvPath"]
	if !ok {
		err := errors.New("Failed to find logic volume path in volume snapshot metadata!")
		log.Error(err)
		return nil, err
	}

	if _, err := d.handler("lvcreate", []string{
		"-n", snapName,
		"-L", size,
		"-p", "r",
		"-s", lvPath,
	}); err != nil {
		log.Error("Failed to create logic volume snapshot:", err)
		return nil, err
	}

	var lvsDir, lvsPath string
	lvsDir, _ = path.Split(lvPath)
	lvsPath = path.Join(lvsDir, snapName)
	// Display and parse some metadata in logic volume snapshot returned.
	lvs, err := d.handler("lvdisplay", []string{lvsPath})
	if err != nil {
		log.Error("Failed to display logic volume snapshot:", err)
		return nil, err
	}
	var lvStatus string
	for _, line := range strings.Split(lvs, "\n") {
		if strings.Contains(line, "LV Status") {
			lvStatus = strings.Fields(line)[2]
		}
	}

	defer func() {
		if snap == nil {
			log.Errorf("create snapshot failed, rollback it")
			d.handler("lvremove", []string{"-f", lvsPath})
		}
	}()

	metadata := map[string]string{"lvsPath": lvsPath}
	if bucket, ok := opt.Metadata["bucket"]; ok {
		mountPoint, info, err := d.AttachSnapshot(id, lvsPath)
		if err != nil {
			return nil, err
		}
		defer d.DetachSnapshot(id, info)

		log.Info("update load snapshot to :", bucket)
		backupId, err := d.uploadSnapshot(mountPoint, bucket)
		if err != nil {
			d.handler("lvremove", []string{"-f", lvsPath})
			return nil, err
		}
		metadata["backupId"] = backupId
		metadata["bucket"] = bucket

	}

	return &model.VolumeSnapshotSpec{
		BaseModel: &model.BaseModel{
			Id: id,
		},
		Name:        opt.GetName(),
		Size:        opt.GetSize(),
		Description: opt.GetDescription(),
		Status:      lvStatus,
		VolumeId:    opt.GetVolumeId(),
		Metadata:    metadata,
	}, nil
}

func (d *Driver) PullSnapshot(snapIdentifier string) (*model.VolumeSnapshotSpec, error) {
	// Display and parse some metadata in logic volume snapshot returned.
	lv, err := d.handler("lvdisplay", []string{snapIdentifier})
	if err != nil {
		log.Error("Failed to display logic volume snapshot:", err)
		return nil, err
	}
	var lvStatus string
	for _, line := range strings.Split(lv, "\n") {
		if strings.Contains(line, "LV Status") {
			lvStatus = strings.Fields(line)[2]
		}
	}

	return &model.VolumeSnapshotSpec{
		Status: lvStatus,
	}, nil
}

func (d *Driver) DeleteSnapshot(opt *pb.DeleteVolumeSnapshotOpts) error {
	lvsPath, ok := opt.GetMetadata()["lvsPath"]
	if !ok {
		err := errors.New("failed to find logic volume snapshot path in volume snapshot " +
			"metadata! ingnore it")
		log.Error(err)
		return nil
	}
	if bucket, ok := opt.Metadata["bucket"]; ok {
		log.Info("remove snapshot in multi-cloud :", bucket)
		if err := d.deleteUploadedSnapshot(opt.Metadata["backupId"], bucket); err != nil {
			return err
		}
	}

	if _, err := d.handler("lvremove", []string{
		"-f", lvsPath,
	}); err != nil {
		log.Error("Failed to remove logic volume:", err)
		return err
	}

	return nil
}

type VolumeGroup struct {
	Name          string
	TotalCapacity int64
	FreeCapacity  int64
	UUID          string
}

func (d *Driver) getVGList() (*[]VolumeGroup, error) {
	info, err := d.handler("vgs", []string{
		"--noheadings", "--nosuffix",
		"--unit=g",
		"-o", "name,size,free,uuid",
	})
	if err != nil {
		return nil, err
	}

	lines := strings.Split(info, "\n")
	var vgs []VolumeGroup
	for _, line := range lines {
		val := strings.Fields(line)
		if len(val) != 4 {
			continue
		}

		capa, _ := strconv.ParseFloat(val[1], 64)
		total := int64(capa)
		capa, _ = strconv.ParseFloat(val[2], 64)
		free := int64(capa)

		vg := VolumeGroup{
			Name:          val[0],
			TotalCapacity: total,
			FreeCapacity:  free,
			UUID:          val[3],
		}
		vgs = append(vgs, vg)
	}
	return &vgs, nil
}

func (d *Driver) ListPools() ([]*model.StoragePoolSpec, error) {

	vgs, err := d.getVGList()
	if err != nil {
		return nil, err
	}

	var pols []*model.StoragePoolSpec
	for _, vg := range *vgs {
		if _, ok := d.conf.Pool[vg.Name]; !ok {
			continue
		}

		pol := &model.StoragePoolSpec{
			BaseModel: &model.BaseModel{
				Id: uuid.NewV5(uuid.NamespaceOID, vg.UUID).String(),
			},
			Name:             vg.Name,
			TotalCapacity:    vg.TotalCapacity,
			FreeCapacity:     vg.FreeCapacity,
			StorageType:      d.conf.Pool[vg.Name].StorageType,
			Extras:           d.conf.Pool[vg.Name].Extras,
			AvailabilityZone: d.conf.Pool[vg.Name].AvailabilityZone,
		}
		if pol.AvailabilityZone == "" {
			pol.AvailabilityZone = "default"
		}
		pols = append(pols, pol)
	}
	return pols, nil
}

func (d *Driver) InitializeSnapshotConnection(opt *pb.CreateSnapshotAttachmentOpts) (*model.ConnectionInfo, error) {
	initiator := opt.HostInfo.GetInitiator()
	if initiator == "" {
		initiator = "ALL"
	}

	hostIP := opt.HostInfo.GetIp()
	if hostIP == "" {
		hostIP = "ALL"
	}

	lvsPath, ok := opt.GetMetadata()["lvsPath"]
	if !ok {
		err := errors.New("Failed to find logic volume path in volume attachment metadata!")
		log.Error(err)
		return nil, err
	}
	var chapAuth []string
	if d.conf.EnableChapAuth {
		chapAuth = []string{utils.RandSeqWithAlnum(20), utils.RandSeqWithAlnum(16)}
	}

	t := targets.NewTarget(d.conf.TgtBindIp, d.conf.TgtConfDir)
	data, err := t.CreateExport(opt.GetSnapshotId(), lvsPath, hostIP, initiator, chapAuth)
	if err != nil {
		log.Error("Failed to initialize snapshot connection of logic volume:", err)
		return nil, err
	}

	return &model.ConnectionInfo{
		DriverVolumeType: ISCSIProtocol,
		ConnectionData:   data,
	}, nil
}

func (d *Driver) TerminateSnapshotConnection(opt *pb.DeleteSnapshotAttachmentOpts) error {
	t := targets.NewTarget(d.conf.TgtBindIp, d.conf.TgtConfDir)
	if err := t.RemoveExport(opt.GetSnapshotId()); err != nil {
		log.Error("Failed to terminate snapshot connection of logic volume:", err)
		return err
	}
	return nil

}

func (d *Driver) CreateVolumeGroup(opt *pb.CreateVolumeGroupOpts, vg *model.VolumeGroupSpec) (*model.VolumeGroupSpec, error) {
	return nil, &model.NotImplementError{"Method CreateVolumeGroup did not implement."}
}

func (d *Driver) UpdateVolumeGroup(opt *pb.UpdateVolumeGroupOpts, vg *model.VolumeGroupSpec, addVolumesRef []*model.VolumeSpec, removeVolumesRef []*model.VolumeSpec) (*model.VolumeGroupSpec, []*model.VolumeSpec, []*model.VolumeSpec, error) {
	return nil, nil, nil, &model.NotImplementError{"Method UpdateVolumeGroup did not implement."}
}

func (d *Driver) DeleteVolumeGroup(opt *pb.DeleteVolumeGroupOpts, vg *model.VolumeGroupSpec, volumes []*model.VolumeSpec) (*model.VolumeGroupSpec, []*model.VolumeSpec, error) {
	return nil, nil, &model.NotImplementError{"Method UpdateVolumeGroup did not implement."}
}

func execCmd(script string, cmd []string) (string, error) {
	log.Infof("Command: %s %s", script, strings.Join(cmd, " "))
	info, err := exec.Command(script, cmd...).Output()
	if err != nil {
		log.Error(info, err.Error())
		return "", err
	}
	log.V(8).Infof("Command Result:\n%s", string(info))
	return string(info), nil
}
