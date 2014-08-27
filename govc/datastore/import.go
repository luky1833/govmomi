/*
Copyright (c) 2014 VMware, Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package datastore

import (
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/govc/cli"
	"github.com/vmware/govmomi/govc/flags"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type import_ struct {
	*flags.DatastoreFlag
	*flags.ResourcePoolFlag
	*flags.SearchFlag

	upload  bool
	import_ bool

	Client       *govmomi.Client
	Datacenter   *govmomi.Datacenter
	Datastore    *govmomi.Datastore
	ResourcePool *govmomi.ResourcePool
}

func init() {
	i := &import_{
		SearchFlag: flags.NewSearchFlag(flags.SearchHosts),
	}

	cli.Register(i)
}

func (cmd *import_) Register(f *flag.FlagSet) {
	f.BoolVar(&cmd.upload, "upload", true, "Upload specified disk")
	f.BoolVar(&cmd.import_, "import", true, "Import specified disk")
}

func (cmd *import_) Process() error { return nil }

func (cmd *import_) Run(f *flag.FlagSet) error {
	var fimport func(importable) error
	var err error

	args := f.Args()
	if len(args) != 1 {
		return errors.New("no file to import")
	}

	file := importable(f.Arg(0))
	switch file.Ext() {
	case ".vmdk":
		fimport = cmd.ImportVMDK
	case ".ovf":
		fimport = cmd.ImportOVF
	default:
		return fmt.Errorf(`unknown type: %s`, file)
	}

	cmd.Client, err = cmd.DatastoreFlag.Client()
	if err != nil {
		return err
	}

	cmd.Datacenter, err = cmd.DatastoreFlag.Datacenter()
	if err != nil {
		return err
	}

	cmd.Datastore, err = cmd.DatastoreFlag.Datastore()
	if err != nil {
		return err
	}

	cmd.ResourcePool, err = cmd.ResourcePoolFlag.ResourcePool()
	if err != nil {
		return err
	}

	if cmd.upload && !file.IsOvf() {
		err = cmd.Upload(file)
		if err != nil {
			return err
		}
	}

	if cmd.import_ {
		return fimport(file)
	}

	return nil
}

func (cmd *import_) Upload(i importable) error {
	u, err := cmd.Datastore.URL(cmd.Client, cmd.Datacenter, i.RemoteVMDK())
	if err != nil {
		return err
	}

	p := soap.DefaultUpload
	if cmd.OutputFlag.TTY {
		ch := make(chan vim25.Progress)
		wg := cmd.ProgressLogger("Uploading... ", ch)
		defer wg.Wait()

		p.ProgressCh = ch
	}

	return cmd.Client.Client.UploadFile(string(i), u, &p)
}

func (cmd *import_) ImportVMDK(i importable) error {
	err := cmd.Copy(i)
	if err != nil {
		return err
	}

	return cmd.Delete(path.Dir(i.RemoteVMDK()))
}

func (cmd *import_) Copy(i importable) error {
	var err error

	dstName := path.Dir(i.RemoteDst())
	srcName := dstName + "-src"

	spec := &configSpec{
		Name:    srcName,
		GuestId: "otherGuest",
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s]", cmd.Datastore.Name()),
		},
	}

	spec.AddDisk(cmd.Datastore, i.RemoteVMDK())

	src, err := cmd.CreateVM(spec)
	if err != nil {
		return err
	}

	dst, err := cmd.CloneVM(src, dstName)
	if err != nil {
		return err
	}

	err = cmd.DestroyVM(src)
	if err != nil {
		return err
	}

	err = cmd.DestroyVM(dst)
	if err != nil {
		return err
	}

	// TODO(PN): Don't use hardcoded file suffixes.
	// TODO(PN): Support arbitrary destination paths, not just top level.
	for _, s := range []string{"", "-flat"} {
		src := i.RemoteDstWithSuffix(s)
		dst := path.Base(src)
		err = cmd.Move(src, dst)
		if err != nil {
			return err
		}
	}

	// Delete source virtual machine directory.
	err = cmd.Delete(dstName)
	if err != nil {
		return err
	}

	return nil
}

func (cmd *import_) Move(src, dst string) error {
	fm := cmd.Client.FileManager()
	dsSrc := cmd.Datastore.Path(src)
	dsDst := cmd.Datastore.Path(dst)
	task, err := fm.MoveDatastoreFile(dsSrc, cmd.Datacenter, dsDst, cmd.Datacenter, true)
	if err != nil {
		return err
	}

	return task.Wait()
}

func (cmd *import_) Delete(path string) error {
	fm := cmd.Client.FileManager()
	dsPath := cmd.Datastore.Path(path)
	task, err := fm.DeleteDatastoreFile(dsPath, cmd.Datacenter)
	if err != nil {
		return err
	}

	return task.Wait()
}

func (cmd *import_) CreateVM(spec *configSpec) (*govmomi.VirtualMachine, error) {
	folders, err := cmd.Datacenter.Folders(cmd.Client)
	if err != nil {
		return nil, err
	}

	task, err := folders.VmFolder.CreateVM(cmd.Client, spec.ToSpec(), cmd.ResourcePool, nil)
	if err != nil {
		return nil, err
	}

	info, err := task.WaitForResult(nil)
	if err != nil {
		return nil, err
	}

	return govmomi.NewVirtualMachine(info.Result.(types.ManagedObjectReference)), nil
}

func (cmd *import_) CloneVM(vm *govmomi.VirtualMachine, name string) (*govmomi.VirtualMachine, error) {
	folders, err := cmd.Datacenter.Folders(cmd.Client)
	if err != nil {
		return nil, err
	}

	spec := types.VirtualMachineCloneSpec{
		Config:   &types.VirtualMachineConfigSpec{},
		Location: types.VirtualMachineRelocateSpec{},
	}

	task, err := vm.Clone(cmd.Client, folders.VmFolder, name, spec)
	if err != nil {
		return nil, err
	}

	info, err := task.WaitForResult(nil)
	if err != nil {
		return nil, err
	}

	return govmomi.NewVirtualMachine(info.Result.(types.ManagedObjectReference)), nil
}

func (cmd *import_) DestroyVM(vm *govmomi.VirtualMachine) error {
	var mvm mo.VirtualMachine

	// TODO(PN): Use `config.hardware` here, see issue #44.
	err := cmd.Client.Properties(vm.Reference(), []string{"config"}, &mvm)
	if err != nil {
		return err
	}

	spec := new(configSpec)
	spec.RemoveDisks(&mvm)

	task, err := vm.Reconfigure(cmd.Client, spec.ToSpec())
	if err != nil {
		return err
	}

	err = task.Wait()
	if err != nil {
		return err
	}

	task, err = vm.Destroy(cmd.Client)
	if err != nil {
		return err
	}

	err = task.Wait()
	if err != nil {
		return err
	}

	return nil
}

// LeaseUpdater consumes an Upload.Progress channel (in) used to update HttpNfcLeaseProgress.
// Progress is forwarded to another channel (out), which can in turn be consumed by the ProgressLogger.
func (cmd *import_) LeaseUpdater(lease *govmomi.HttpNfcLease, in <-chan vim25.Progress, out chan<- vim25.Progress) *sync.WaitGroup {
	var wg sync.WaitGroup

	go func() {
		var p vim25.Progress
		var ok bool
		var err error
		var percent int

		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		defer wg.Done()

		for ok = true; ok && err == nil; {
			select {
			case p, ok = <-in:
				if !ok {
					break
				}
				percent = int(p.Percentage())
				err = p.Error()
				out <- p // Forward to the ProgressLogger
			case <-tick.C:
				// From the vim api HttpNfcLeaseProgress(percent) doc, percent ==
				// "Completion status represented as an integer in the 0-100 range."
				// Always report the current value of percent,
				// as it will renew the lease even if the value hasn't changed or is 0
				err = lease.HttpNfcLeaseProgress(cmd.Client, percent)
			}
		}
	}()

	wg.Add(1)

	return &wg
}

func (cmd *import_) nfcUpload(lease *govmomi.HttpNfcLease, file string, u *url.URL, create bool) error {
	in := make(chan vim25.Progress)

	out := make(chan vim25.Progress)

	wg := cmd.LeaseUpdater(lease, in, out)

	pwg := cmd.ProgressLogger("Uploading... ", out)

	// defer queue is LIFO..
	defer pwg.Wait() // .... 3) wait for ProgressLogger to return
	defer close(out) // .... 2) propagate close to chained channel
	defer wg.Wait()  // .... 1) wait for Progress channel to close

	opts := soap.Upload{
		Type:       "application/x-vnd.vmware-streamVmdk",
		Method:     "POST",
		ProgressCh: in,
	}

	if create {
		opts.Method = "PUT"
	}

	return cmd.Client.Client.UploadFile(file, u, &opts)
}

func (cmd *import_) ImportOVF(i importable) error {
	c := cmd.Client

	desc, err := ioutil.ReadFile(string(i))
	if err != nil {
		return err
	}

	// extract name from .ovf for use as VM name
	ovf := struct {
		VirtualSystem struct {
			Name string
		}
	}{}

	if err := xml.Unmarshal(desc, &ovf); err != nil {
		return fmt.Errorf("failed to parse ovf: %s", err.Error())
	}

	cisp := types.OvfCreateImportSpecParams{
		EntityName: ovf.VirtualSystem.Name,
		OvfManagerCommonParams: types.OvfManagerCommonParams{
			Locale: "US",
		},
	}

	spec, err := c.OvfManager().CreateImportSpec(string(desc), cmd.ResourcePool, cmd.Datastore, cisp)
	if err != nil {
		return err
	}

	if spec.Error != nil {
		return errors.New(spec.Error[0].LocalizedMessage)
	}

	if spec.Warning != nil {
		for _, w := range spec.Warning {
			fmt.Printf("Warning: %s\n", w.LocalizedMessage)
		}
	}

	// TODO: ImportSpec may have unitNumber==0, but this field is optional in the wsdl
	// and hence omitempty in the struct tag; but unitNumber is required for certain devices.
	s := &spec.ImportSpec.(*types.VirtualMachineImportSpec).ConfigSpec
	for _, d := range s.DeviceChange {
		n := &d.GetVirtualDeviceConfigSpec().Device.GetVirtualDevice().UnitNumber
		if *n == 0 {
			*n = -1
		}
	}

	host, err := cmd.HostSystem()
	if err != nil {
		return err
	}

	// TODO: need a folder option
	folders, err := cmd.Datacenter.Folders(c)
	if err != nil {
		return err
	}
	folder := &folders.VmFolder

	lease, err := cmd.ResourcePool.ImportVApp(c, spec.ImportSpec, folder, host)
	if err != nil {
		return err
	}

	info, err := lease.Wait(c)
	if err != nil {
		return err
	}

	for _, device := range info.DeviceUrl {
		for _, item := range spec.FileItem {
			if device.ImportKey != item.DeviceId {
				continue
			}

			file := filepath.Join(i.Dir(), item.Path)

			u, err := c.Client.ParseURL(device.Url)
			if err != nil {
				return err
			}

			err = cmd.nfcUpload(lease, file, u, item.Create)
			if err != nil {
				return err
			}
		}
	}

	return lease.HttpNfcLeaseComplete(c)
}

type importable string

func (i importable) Ext() string {
	return strings.ToLower(path.Ext(string(i)))
}

func (i importable) Base() string {
	return path.Base(string(i))
}

func (i importable) Dir() string {
	return path.Dir(string(i))
}

func (i importable) BaseClean() string {
	b := path.Base(string(i))
	e := path.Ext(string(i))
	return b[:len(b)-len(e)]
}

func (i importable) RemoteVMDK() string {
	bc := i.BaseClean()
	return fmt.Sprintf("%s-vmdk/%s.vmdk", bc, bc)
}

func (i importable) RemoteDst() string {
	bc := i.BaseClean()
	return fmt.Sprintf("%s/%s.vmdk", bc, bc)
}

func (i importable) RemoteDstWithSuffix(s string) string {
	bc := i.BaseClean()
	return fmt.Sprintf("%s/%s%s.vmdk", bc, bc, s)
}

func (i importable) IsOvf() bool {
	return i.Ext() == ".ovf"
}

type configSpec types.VirtualMachineConfigSpec

func (c *configSpec) ToSpec() types.VirtualMachineConfigSpec {
	return types.VirtualMachineConfigSpec(*c)
}

func (c *configSpec) AddChange(d types.BaseVirtualDeviceConfigSpec) {
	c.DeviceChange = append(c.DeviceChange, d)
}

func (c *configSpec) AddDisk(ds *govmomi.Datastore, path string) {
	controller := &types.VirtualLsiLogicController{
		types.VirtualSCSIController{
			SharedBus: types.VirtualSCSISharingNoSharing,
			VirtualController: types.VirtualController{
				BusNumber: 0,
				VirtualDevice: types.VirtualDevice{
					Key: -1,
				},
			},
		},
	}

	controllerSpec := &types.VirtualDeviceConfigSpec{
		Device:    controller,
		Operation: types.VirtualDeviceConfigSpecOperationAdd,
	}

	c.AddChange(controllerSpec)

	disk := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Key:           -1,
			ControllerKey: -1,
			UnitNumber:    -1,
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
					FileName: ds.Path(path),
				},
				DiskMode:        string(types.VirtualDiskModePersistent),
				ThinProvisioned: true,
			},
		},
	}

	diskSpec := &types.VirtualDeviceConfigSpec{
		Device:    disk,
		Operation: types.VirtualDeviceConfigSpecOperationAdd,
	}

	c.AddChange(diskSpec)
}

func (c *configSpec) RemoveDisks(vm *mo.VirtualMachine) {
	for _, d := range vm.Config.Hardware.Device {
		switch device := d.(type) {
		case *types.VirtualDisk:
			removeOp := &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationRemove,
				Device:    device,
			}

			c.AddChange(removeOp)
		}
	}
}