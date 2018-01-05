/*
Copyright 2016 The Rook Authors. All rights reserved.

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
package osd

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha1"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/mon"
	"github.com/rook/rook/pkg/daemon/ceph/test"
	"github.com/rook/rook/pkg/operator/k8sutil"
	testop "github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOSDAgentWithDevicesFilestore(t *testing.T) {
	testOSDAgentWithDevicesHelper(t, rookalpha.StoreConfig{StoreType: Filestore})
}

func TestOSDAgentWithDevicesBluestore(t *testing.T) {
	testOSDAgentWithDevicesHelper(t, rookalpha.StoreConfig{StoreType: Bluestore})
}

func testOSDAgentWithDevicesHelper(t *testing.T, storeConfig rookalpha.StoreConfig) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	agent, executor := createTestAgent(t, "sdx,sdy", configDir, &storeConfig)

	startCount := 0
	executor.MockStartExecuteCommand = func(debug bool, name string, command string, args ...string) (*exec.Cmd, error) {
		logger.Infof("START %d for %s. %s %+v", startCount, name, command, args)
		cmd := &exec.Cmd{Args: append([]string{command}, args...)}

		switch {
		case startCount < 2:
			assert.Equal(t, "ceph-osd", command)
		default:
			assert.Fail(t, fmt.Sprintf("unexpected case %d", startCount))
		}
		startCount++
		return cmd, nil
	}

	execCount := 0
	executor.MockExecuteCommand = func(debug bool, name string, command string, args ...string) error {
		logger.Infof("RUN %d: %s %+v", execCount, command, args)
		parts := strings.Split(name, " ")
		nameSuffix := parts[0]
		if len(parts) > 1 {
			nameSuffix = parts[1]
		}

		if storeConfig.StoreType == Bluestore {
			switch {
			case execCount == 0: // first exec is the osd mkfs for sdx
				assert.Equal(t, "--mkfs", args[0])
				createTestKeyring(t, configDir, args)
			case execCount == 1: // all remaining execs are for partitioning sdy then mkfs sdy
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "--zap-all", args[0])
				assert.Equal(t, "/dev/"+nameSuffix, args[1])
			case execCount == 2:
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "--clear", args[0])
				assert.Equal(t, "/dev/"+nameSuffix, args[2])
			case execCount == 3:
				// the partitioning for sdy.
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "/dev/"+nameSuffix, args[10])
			case execCount == 4:
				// the osd mkfs for sdy bluestore
				assert.Equal(t, "--mkfs", args[0])
				createTestKeyring(t, configDir, args)
			default:
				assert.Fail(t, fmt.Sprintf("unexpected case %d", execCount))
			}
		} else if storeConfig.StoreType == Filestore {
			switch {
			case execCount == 0:
				// first exec is the remounting of sdx because its partitions were created previously, we just need to remount it
				// note this only happens for filestore (not bluestore)
				assert.Equal(t, "mount", command)
			case execCount == 1:
				// the osd mkfs for sdx
				assert.Equal(t, "--mkfs", args[0])
				createTestKeyring(t, configDir, args)
			case execCount == 2: // all remaining execs are for partitioning sdy then mkfs sdy
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "--zap-all", args[0])
				assert.Equal(t, "/dev/"+nameSuffix, args[1])
			case execCount == 3:
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "--clear", args[0])
				assert.Equal(t, "/dev/"+nameSuffix, args[2])
			case execCount == 4:
				// the partitioning for sdy.
				assert.Equal(t, "sgdisk", command)
				assert.Equal(t, "/dev/"+nameSuffix, args[4])
			case execCount == 5:
				// mkfs.ext4 for sdy filestore
				assert.Equal(t, "mkfs.ext4", command)
			case execCount == 6:
				// the mount for sdy filestore
				assert.Equal(t, "mount", command)
			case execCount == 7:
				// the osd mkfs for sdy filestore
				assert.Equal(t, "--mkfs", args[0])
				createTestKeyring(t, configDir, args)
			default:
				assert.Fail(t, fmt.Sprintf("unexpected case %d", execCount))
			}
		}

		execCount++
		return nil
	}

	outputExecCount := 0
	executor.MockExecuteCommandWithOutputFile = func(debug bool, actionName string, command string, outFileArg string, args ...string) (string, error) {
		logger.Infof("OUTPUT %d for %s. %s %+v", outputExecCount, actionName, command, args)
		outputExecCount++
		if args[0] == "auth" && args[1] == "get-or-create-key" {
			return "{\"key\":\"mysecurekey\"}", nil
		}
		if args[0] == "osd" && args[1] == "create" {
			return "{\"osdid\":3.0}", nil
		}
		return "", nil
	}
	executor.MockExecuteCommandWithOutput = func(debug bool, actionName string, command string, args ...string) (string, error) {
		logger.Infof("OUTPUT %d for %s. %s %+v", outputExecCount, actionName, command, args)
		outputExecCount++
		if strings.HasPrefix(actionName, "lsblk /dev/disk/by-partuuid") {
			// this is a call to get device properties so we figure out CRUSH weight, which should only be done for Bluestore
			// (Filestore uses Statfs since it has a mounted filesystem)
			assert.Equal(t, Bluestore, storeConfig.StoreType)
			return `SIZE="1234567890" TYPE="part"`, nil
		}
		return "", nil
	}

	context := &clusterd.Context{
		Executor:  executor,
		ConfigDir: configDir,
	}

	// Set sdx as already having an assigned osd id, a UUID and saved to the partition scheme.
	// The other device (sdy) will go through id selection, which is mocked in the createTestAgent method to return an id of 3.
	_, sdxUUID := mockPartitionSchemeEntry(t, 23, "sdx", &storeConfig, agent.kv, agent.nodeName)

	// note only sdx already has a UUID (it's been through partitioning)
	context.Devices = []*clusterd.LocalDisk{
		{Name: "sdx", Size: 1234567890, UUID: sdxUUID},
		{Name: "sdy", Size: 1234567890},
	}
	devices := &DeviceOsdMapping{Entries: map[string]*DeviceOsdIDEntry{
		"sdx": {Data: -1},
		"sdy": {Data: -1},
	}}
	err = agent.configureDevices(context, devices)
	assert.Nil(t, err)

	assert.Equal(t, int32(0), agent.configCounter)
	assert.Equal(t, 2, startCount) // 2 OSD procs should be started
	assert.Equal(t, 2, len(agent.osdProc), fmt.Sprintf("procs=%+v", agent.osdProc))

	if storeConfig.StoreType == Bluestore {
		assert.Equal(t, 11, outputExecCount) // Bluestore has 2 extra output exec calls to get device properties of each device to determine CRUSH weight
		assert.Equal(t, 5, execCount)        // 1 osd mkfs for sdx, 3 partition steps for sdy, 1 osd mkfs for sdy
	} else {
		assert.Equal(t, 9, outputExecCount)
		assert.Equal(t, 8, execCount) // 1 for remount sdx, 1 osd mkfs for sdx, 3 partition steps for sdy, 1 mkfs for sdy, 1 mount for sdy, 1 osd mkfs for sdy
	}
}

func TestOSDAgentNoDevices(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestOSDAgentNoDevices")
	require.NoError(t, err)
	defer os.RemoveAll(configDir)

	os.MkdirAll(filepath.Join(configDir, "osd3"), 0744)

	// create a test OSD agent with no devices specified
	agent, executor := createTestAgent(t, "", configDir, nil)

	startCount := 0
	executor.MockStartExecuteCommand = func(debug bool, name string, command string, args ...string) (*exec.Cmd, error) {
		logger.Infof("StartExecuteCommand: %s %v", command, args)
		startCount++
		cmd := &exec.Cmd{Args: append([]string{command}, args...)}
		return cmd, nil
	}

	runCount := 0
	executor.MockExecuteCommand = func(debug bool, name string, command string, args ...string) error {
		logger.Infof("ExecuteCommand: %s %v", command, args)
		runCount++
		createTestKeyring(t, configDir, args)
		return nil
	}

	execWithOutputFileCount := 0
	executor.MockExecuteCommandWithOutputFile = func(debug bool, actionName string, command string, outFileArg string, args ...string) (string, error) {
		logger.Infof("ExecuteCommandWithOutputFile: %s %v", command, args)
		execWithOutputFileCount++
		return "{\"key\":\"mysecurekey\", \"osdid\":3.0}", nil
	}

	execWithOutputCount := 0
	executor.MockExecuteCommandWithOutput = func(debug bool, actionName string, command string, arg ...string) (string, error) {
		logger.Infof("ExecuteCommandWithOutput: %s %v", command, arg)
		execWithOutputCount++
		return "", nil
	}

	// set up expected ProcManager commands
	context := &clusterd.Context{
		Devices:   []*clusterd.LocalDisk{},
		Executor:  executor,
		ConfigDir: configDir,
	}
	dirs := map[string]int{
		filepath.Join(configDir, "sdx"): -1,
		filepath.Join(configDir, "sdy"): -1,
	}
	err = agent.configureDirs(context, dirs)
	assert.Nil(t, err)
	assert.Equal(t, 2, runCount)
	assert.Equal(t, 2, startCount)
	assert.Equal(t, 6, execWithOutputFileCount)
	assert.Equal(t, 2, execWithOutputCount)
	assert.Equal(t, 1, len(agent.osdProc))
}

func createTestAgent(t *testing.T, devices, configDir string, storeConfig *rookalpha.StoreConfig) (*OsdAgent, *exectest.MockExecutor) {
	location := "root=here"
	forceFormat := false
	if storeConfig == nil {
		storeConfig = &rookalpha.StoreConfig{StoreType: Bluestore}
	}

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(debug bool, actionName string, command string, outFileArg string, args ...string) (string, error) {
			return "{\"key\":\"mysecurekey\", \"osdid\":3.0}", nil
		},
	}
	cluster := &mon.ClusterInfo{Name: "myclust"}
	agent := NewAgent(
		&clusterd.Context{Executor: executor, Clientset: testop.New(1)},
		devices, false, "", "", forceFormat, location, *storeConfig, cluster, "myhost", mockKVStore())

	return agent, executor
}

func createTestKeyring(t *testing.T, configRoot string, args []string) {
	var configDir string
	if len(args) > 5 && strings.HasPrefix(args[5], "--id") {
		configDir = filepath.Join(configRoot, "osd") + args[5][5:]
		err := os.MkdirAll(configDir, 0744)
		assert.Nil(t, err)
		err = ioutil.WriteFile(path.Join(configDir, "keyring"), []byte("mykeyring"), 0644)
		assert.Nil(t, err)
	}
}

func TestGetPartitionPerfScheme(t *testing.T) {
	configDir, _ := ioutil.TempDir("", "")
	defer os.RemoveAll(configDir)
	context := &clusterd.Context{Devices: []*clusterd.LocalDisk{}, ConfigDir: configDir}
	test.CreateConfigDir(configDir)

	// 3 disks: 2 for data and 1 for the metadata of both disks (2 WALs and 2 DBs)
	a := &OsdAgent{desiredDevices: []string{"sda", "sdb"}, metadataDevice: "sdc", kv: mockKVStore(), nodeName: "a"}
	context.Devices = []*clusterd.LocalDisk{
		{Name: "sda", Size: 107374182400}, // 100 GB
		{Name: "sdb", Size: 107374182400}, // 100 GB
		{Name: "sdc", Size: 44158681088},  // 1 MB (starting offset) + 2 * (576 MB + 20 GB) = 41.125 GB
	}
	clusterInfo := &mon.ClusterInfo{Name: "myclust"}
	a.cluster = clusterInfo

	// mock monitor command to return an osd ID when the client registers/creates an osd
	currOsdID := 10
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(debug bool, actionName string, command string, outFileArg string, args ...string) (string, error) {
			switch {
			case args[0] == "osd" && args[1] == "create":
				currOsdID++
				return fmt.Sprintf(`{"osdid": %d}`, currOsdID), nil
			}
			return "", fmt.Errorf("unexpected command '%v'", args)
		},
		MockExecuteCommandWithOutput: func(debug bool, actionName string, command string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if command == "lsblk" {
				if args[0] == "/dev/sda" {
					return `NAME="sda" SIZE="107374182400" TYPE="disk" PKNAME=""`, nil
				}
				if args[0] == "/dev/sdb" {
					return `NAME="sdb" SIZE="107374182400" TYPE="disk" PKNAME=""`, nil
				}
				if args[0] == "/dev/sdc" {
					return `NAME="sdc" SIZE="44158681088" TYPE="disk" PKNAME=""`, nil
				}
			}
			if command == "blkid" {
				return "", nil
			}
			if command == "df" {
				return "", nil
			}
			return "", fmt.Errorf("unexpected command %s %v", command, args)
		},
	}
	context.Executor = executor

	devices, err := getAvailableDevices(context, "sda,sdb", "sdc", false)
	assert.Nil(t, err)
	scheme, err := a.getPartitionPerfScheme(context, devices)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(scheme.Entries))

	// verify the metadata entries, they should be on sdc and there should be 2 of them (2 per OSD)
	require.NotNil(t, scheme.Metadata)
	assert.Equal(t, "sdc", scheme.Metadata.Device)
	assert.Equal(t, 4, len(scheme.Metadata.Partitions))

	// verify the first entry in the performance partition scheme.  note that the block device will either be sda or
	// sdb because ordering of map traversal in golang isn't guaranteed.  Ensure that the first is either sda or sdb
	// and that the second is the other one.
	entry := scheme.Entries[0]
	assert.Equal(t, 11, entry.ID)
	firstBlockDevice := entry.Partitions[BlockPartitionType].Device
	assert.True(t, firstBlockDevice == "sda" || firstBlockDevice == "sdb", firstBlockDevice)
	verifyPartitionEntry(t, entry.Partitions[BlockPartitionType], firstBlockDevice, -1, 1)
	verifyPartitionEntry(t, entry.Partitions[WalPartitionType], "sdc", WalDefaultSizeMB, 1)
	verifyPartitionEntry(t, entry.Partitions[DatabasePartitionType], "sdc", DBDefaultSizeMB, 577)

	// verify the second entry in the scheme.  Note the comment above about sda vs. sdb ordering.
	entry = scheme.Entries[1]
	assert.Equal(t, 12, entry.ID)
	var secondBlockDevice string
	if firstBlockDevice == "sda" {
		secondBlockDevice = "sdb"
	} else {
		secondBlockDevice = "sda"
	}
	verifyPartitionEntry(t, entry.Partitions[BlockPartitionType], secondBlockDevice, -1, 1)
	verifyPartitionEntry(t, entry.Partitions[WalPartitionType], "sdc", WalDefaultSizeMB, 21057)
	verifyPartitionEntry(t, entry.Partitions[DatabasePartitionType], "sdc", DBDefaultSizeMB, 21633)
}

func TestGetPartitionSchemeDiskInUse(t *testing.T) {
	configDir, err := ioutil.TempDir("", "TestGetPartitionPerfSchemeDiskInUse")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(debug bool, actionName string, command string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if command == "lsblk" {
				if args[0] == "/dev/sda" {
					return `NAME="sda" SIZE="20971520000" TYPE="disk" PKNAME=""
					NAME="sda1" SIZE="19921895424" TYPE="part" PKNAME="sda"
					NAME="sda2" SIZE="1048576000" TYPE="part" PKNAME="sda"`, nil
				}
			}
			if command == "blkid" {
				return "", nil
			}
			if command == "df" {
				return "", nil
			}
			return "", fmt.Errorf("unexpected command %s %v", command, args)
		},
	}
	context := &clusterd.Context{
		Devices:   []*clusterd.LocalDisk{},
		ConfigDir: configDir,
		Executor:  executor,
	}

	a := &OsdAgent{desiredDevices: []string{"sda"}, kv: mockKVStore()}
	_, sdaUUID := mockPartitionSchemeEntry(t, 1, "sda", nil, a.kv, a.nodeName)

	context.Devices = []*clusterd.LocalDisk{
		{Name: "sda", Size: 107374182400, UUID: sdaUUID}, // 100 GB
	}

	// get the partition scheme based on the available devices.  Since sda is already in use, the partition
	// scheme returned should reflect that.
	devices, err := getAvailableDevices(context, "sda", "", false)
	scheme, err := a.getPartitionPerfScheme(context, devices)
	assert.Nil(t, err)

	// the partition scheme should have a single entry for osd 1 on sda and it should have collocated data and metadata
	assert.NotNil(t, scheme)
	assert.Equal(t, 1, len(scheme.Entries))
	assert.Equal(t, 1, scheme.Entries[0].ID)
	assert.Equal(t, 3, len(scheme.Entries[0].Partitions))
	for _, p := range scheme.Entries[0].Partitions {
		assert.Equal(t, "sda", p.Device)
		assert.Equal(t, sdaUUID, p.DiskUUID)
	}

	// there should be no dedicated metadata partitioning because sda has osd 1 collocated on it
	assert.Nil(t, scheme.Metadata)
}

func TestGetPartitionSchemeDiskNameChanged(t *testing.T) {
	configDir, err := ioutil.TempDir("", "TestGetPartitionPerfSchemeDiskNameChanged")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(debug bool, actionName string, command string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if command == "lsblk" {
				if args[0] == "/dev/sda-changed" {
					return `NAME="sda" SIZE="20971520000" TYPE="disk" PKNAME=""
					NAME="sda1" SIZE="19921895424" TYPE="part" PKNAME="sda"
					NAME="sda2" SIZE="1048576000" TYPE="part" PKNAME="sda"`, nil
				}
				if args[0] == "/dev/nvme01-changed" {
					return `NAME="nvme01-changed" SIZE="20971520000" TYPE="disk" PKNAME=""
					NAME="nvme01-changed1" SIZE="19921895424" TYPE="part" PKNAME="nvme01-changed"
					NAME="nvme01-changed2" SIZE="1048576000" TYPE="part" PKNAME="nvme01-changed"`, nil
				}
			}
			if command == "blkid" {
				return "", nil
			}
			if command == "df" {
				return "", nil
			}
			return "", fmt.Errorf("unexpected command %s %v", command, args)
		},
	}
	context := &clusterd.Context{
		Devices:   []*clusterd.LocalDisk{},
		ConfigDir: configDir,
		Executor:  executor,
	}

	// mock the currently discovered hardware, note the device names have changed (e.g., across reboots) but their UUIDs are always static
	a := &OsdAgent{desiredDevices: []string{"sda-changed"}, kv: mockKVStore()}

	// setup an existing partition schme with metadata on nvme01 and data on sda
	_, metadataUUID, sdaUUID := mockDistributedPartitionScheme(t, 1, "nvme01", "sda", a.kv, a.nodeName)

	context.Devices = []*clusterd.LocalDisk{
		{Name: "nvme01-changed", Size: 107374182400, UUID: metadataUUID},
		{Name: "sda-changed", Size: 107374182400, UUID: sdaUUID},
	}

	// get the current partition scheme.  This should notice that the device names changed and update the
	// partition scheme to have the latest device names
	devices, err := getAvailableDevices(context, "sda-changed", "nvme01", false)
	scheme, err := a.getPartitionPerfScheme(context, devices)
	assert.Nil(t, err)
	require.NotNil(t, scheme)
	assert.Equal(t, "sda-changed", scheme.Entries[0].Partitions[BlockPartitionType].Device)
	assert.Equal(t, "nvme01", scheme.Metadata.Device)
	assert.Equal(t, "nvme01", scheme.Entries[0].Partitions[WalPartitionType].Device)
	assert.Equal(t, "nvme01", scheme.Entries[0].Partitions[DatabasePartitionType].Device)
}

func verifyPartitionEntry(t *testing.T, actual *PerfSchemePartitionDetails, expectedDevice string,
	expectedSize int, expectedOffset int) {

	assert.Equal(t, expectedDevice, actual.Device)
	assert.Equal(t, expectedSize, actual.SizeMB)
	assert.Equal(t, expectedOffset, actual.OffsetMB)
}

func mockPartitionSchemeEntry(t *testing.T, osdID int, device string, storeConfig *rookalpha.StoreConfig,
	kv *k8sutil.ConfigMapKVStore, nodeName string) (entry *PerfSchemeEntry, diskUUID string) {

	if storeConfig == nil {
		storeConfig = &rookalpha.StoreConfig{StoreType: Bluestore}
	}

	entry = NewPerfSchemeEntry(storeConfig.StoreType)
	entry.ID = osdID
	entry.OsdUUID = uuid.Must(uuid.NewRandom())
	PopulateCollocatedPerfSchemeEntry(entry, device, *storeConfig)
	scheme := NewPerfScheme()
	scheme.Entries = append(scheme.Entries, entry)
	err := scheme.SaveScheme(kv, getConfigStoreName(nodeName))
	assert.Nil(t, err)

	// figure out what random UUID got assigned to the device
	for _, p := range entry.Partitions {
		diskUUID = p.DiskUUID
		break
	}
	assert.NotEqual(t, "", diskUUID)

	return entry, diskUUID
}

func mockDistributedPartitionScheme(t *testing.T, osdID int, metadataDevice, device string,
	kv *k8sutil.ConfigMapKVStore, nodeName string) (*PerfScheme, string, string) {

	scheme := NewPerfScheme()
	scheme.Metadata = NewMetadataDeviceInfo(metadataDevice)

	entry := NewPerfSchemeEntry(Bluestore)
	entry.ID = osdID
	entry.OsdUUID = uuid.Must(uuid.NewRandom())

	PopulateDistributedPerfSchemeEntry(entry, device, scheme.Metadata, rookalpha.StoreConfig{})
	scheme.Entries = append(scheme.Entries, entry)
	err := scheme.SaveScheme(kv, getConfigStoreName(nodeName))
	assert.Nil(t, err)

	// return the full partition scheme, the metadata device UUID and the data device UUID
	return scheme, scheme.Metadata.DiskUUID, entry.Partitions[BlockPartitionType].DiskUUID
}

func mockKVStore() *k8sutil.ConfigMapKVStore {
	clientset := testop.New(1)
	return k8sutil.NewConfigMapKVStore("myns", clientset)
}