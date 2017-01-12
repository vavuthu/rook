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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	testceph "github.com/rook/rook/pkg/cephmgr/client/test"
	"github.com/rook/rook/pkg/cephmgr/osd/partition"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/clusterd/inventory"
	"github.com/rook/rook/pkg/util"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/rook/rook/pkg/util/sys"
	"github.com/stretchr/testify/assert"
)

func TestGetOSDInfo(t *testing.T) {
	// error when no info is found on disk
	config := &osdConfig{rootPath: "/tmp"}

	err := loadOSDInfo(config)
	assert.NotNil(t, err)

	// write the info to disk
	whoFile := "/tmp/whoami"
	ioutil.WriteFile(whoFile, []byte("23"), 0644)
	defer os.Remove(whoFile)
	fsidFile := "/tmp/fsid"
	testUUID, _ := uuid.NewUUID()
	ioutil.WriteFile(fsidFile, []byte(testUUID.String()), 0644)
	defer os.Remove(fsidFile)

	// check the successful osd info
	err = loadOSDInfo(config)
	assert.Nil(t, err)
	assert.Equal(t, 23, config.id)
	assert.Equal(t, testUUID, config.uuid)
}

func TestOSDBootstrap(t *testing.T) {
	clusterName := "mycluster"
	targetPath := getBootstrapOSDKeyringPath("/tmp", clusterName)
	defer os.Remove(targetPath)

	factory := &testceph.MockConnectionFactory{}
	conn, _ := factory.NewConnWithClusterAndUser(clusterName, "user")
	conn.(*testceph.MockConnection).MockMonCommand = func(buf []byte) (buffer []byte, info string, err error) {
		response := "{\"key\":\"mysecurekey\"}"
		logger.Infof("Returning: %s", response)
		return []byte(response), "", nil
	}

	err := createOSDBootstrapKeyring(conn, "/tmp", clusterName)
	assert.Nil(t, err)

	contents, err := ioutil.ReadFile(targetPath)
	assert.Nil(t, err)
	assert.NotEqual(t, -1, strings.Index(string(contents), "[client.bootstrap-osd]"))
	assert.NotEqual(t, -1, strings.Index(string(contents), "key = mysecurekey"))
	assert.NotEqual(t, -1, strings.Index(string(contents), "caps mon = \"allow profile bootstrap-osd\""))
}

func TestOverwriteRookOwnedPartitions(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestOverwriteRookOwnedPartitions")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	nodeID := "node123"
	etcdClient := util.NewMockEtcdClient()

	// set up mock execute so we can verify the partitioning happens on sda
	execCount := 0
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommand = func(name string, command string, args ...string) error {
		logger.Infof("RUN %d for '%s'. %s %+v", execCount, name, command, args)
		assert.Equal(t, "sgdisk", command)
		switch execCount {
		case 0:
			assert.Equal(t, []string{"--zap-all", "/dev/sda"}, args)
		case 1:
			assert.Equal(t, []string{"--clear", "--mbrtogpt", "/dev/sda"}, args)
		case 2:
			assert.Equal(t, 11, len(args))
			assert.Equal(t, "--change-name=1:ROOK-OSD1-WAL", args[1])
			assert.Equal(t, "--change-name=2:ROOK-OSD1-DB", args[4])
			assert.Equal(t, "--change-name=3:ROOK-OSD1-BLOCK", args[7])
			assert.Equal(t, "/dev/sda", args[10])
		}
		execCount++
		return nil
	}

	// set up a mock function to return "rook owned" partitions on the device and it does not have a filesystem
	outputExecCount := 0
	executor.MockExecuteCommandWithOutput = func(name string, command string, args ...string) (string, error) {
		logger.Infof("OUTPUT %d for %s. %s %+v", outputExecCount, name, command, args)
		var output string
		switch outputExecCount {
		case 0, 1: // we'll call this twice, once explicitly below to verify rook owns the partitions and a 2nd time within formatDevice
			assert.Equal(t, command, "lsblk")
			output = `NAME="sda" SIZE="65" TYPE="disk" PKNAME="" PARTLABEL=""
NAME="sda1" SIZE="30" TYPE="part" PKNAME="sda" PARTLABEL="ROOK-OSD0-WAL"
NAME="sda2" SIZE="10" TYPE="part" PKNAME="sda" PARTLABEL="ROOK-OSD0-DB"
NAME="sda3" SIZE="20" TYPE="part" PKNAME="sda" PARTLABEL="ROOK-OSD0-BLOCK"`
		case 2:
			assert.Equal(t, command, "df")
			output = ""
		}
		outputExecCount++
		return output, nil
	}

	// set up a partition scheme entry for sda (collocated metadata and data)
	entry := partition.NewPerfSchemeEntry()
	entry.ID = 1
	entry.OsdUUID = uuid.Must(uuid.NewRandom())
	partition.PopulateCollocatedPerfSchemeEntry(entry, "sda", partition.BluestoreConfig{})

	context := &clusterd.Context{EtcdClient: etcdClient, Executor: executor, NodeID: nodeID,
		ConfigDir: configDir, Inventory: createInventory()}
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sda", Size: 65},
	}
	config := &osdConfig{configRoot: configDir, rootPath: filepath.Join(configDir, "osd1"), id: entry.ID,
		uuid: entry.OsdUUID, dir: false, partitionScheme: entry}

	// ensure that our mocking makes it look like rook owns the partitions on sda
	partitions, _, err := sys.GetDevicePartitions("sda", context.Executor)
	assert.Nil(t, err)
	assert.True(t, rookOwnsPartitions(partitions))

	// try to format the device.  even though the device has existing partitions, they are owned by rook, so it is safe
	// to format and the format/partitioning will happen.
	err = formatDevice(context, config, false)
	assert.Nil(t, err)
	assert.Equal(t, 3, execCount)
	assert.Equal(t, 3, outputExecCount)
}

func TestPartitionBluestoreMetadata(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestPartitionBluestoreMetadata")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	nodeID := "node123"
	etcdClient := util.NewMockEtcdClient()

	execCount := 0
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommand = func(name string, command string, args ...string) error {
		logger.Infof("RUN %d for '%s'. %s %+v", execCount, name, command, args)
		assert.Equal(t, "sgdisk", command)
		switch execCount {
		case 0:
			assert.Equal(t, []string{"--zap-all", "/dev/sda"}, args)
		case 1:
			assert.Equal(t, []string{"--clear", "--mbrtogpt", "/dev/sda"}, args)
		case 2:
			assert.Equal(t, 14, len(args))
			assert.Equal(t, "--change-name=1:ROOK-OSD1-WAL", args[1])
			assert.Equal(t, "--change-name=2:ROOK-OSD1-DB", args[4])
			assert.Equal(t, "--change-name=3:ROOK-OSD2-WAL", args[7])
			assert.Equal(t, "--change-name=4:ROOK-OSD2-DB", args[10])
		}
		execCount++
		return nil
	}

	context := &clusterd.Context{EtcdClient: etcdClient, Executor: executor, NodeID: nodeID, ConfigDir: configDir}

	// create metadata partition information for 2 OSDs (sdb, sdc) storing their metadata on device sda
	bluestoreConfig := partition.BluestoreConfig{WalSizeMB: 1, DatabaseSizeMB: 2}
	metadata := partition.NewMetadataDeviceInfo("sda")

	e1 := partition.NewPerfSchemeEntry()
	e1.ID = 1
	e1.OsdUUID = uuid.Must(uuid.NewRandom())
	partition.PopulateDistributedPerfSchemeEntry(e1, "sdb", metadata, bluestoreConfig)

	e2 := partition.NewPerfSchemeEntry()
	e2.ID = 2
	e2.OsdUUID = uuid.Must(uuid.NewRandom())
	partition.PopulateDistributedPerfSchemeEntry(e2, "sdc", metadata, bluestoreConfig)

	// perform the metadata device partition
	err = partitionBluestoreMetadata(context, metadata, configDir)
	assert.Nil(t, err)
	assert.Equal(t, 3, execCount)

	// verify that the metadata device has been associated with the OSDs that are storing their metadata on it,
	// e.g. OSDs 1 and 2
	desiredIDsRaw := etcdClient.GetValue(
		fmt.Sprintf("/rook/services/ceph/osd/desired/node123/device/%s/osd-id-metadata", metadata.DiskUUID))
	desiredIds := strings.Split(desiredIDsRaw, ",")
	assert.True(t, util.CreateSet(desiredIds).Equals(util.CreateSet([]string{"1", "2"})))
}

func TestPartitionBluestoreOSD(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestPartitionBluestoreOSD")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	nodeID := "node123"
	etcdClient := util.NewMockEtcdClient()

	// setup the mock executor to validate the calls to partition the device
	execCount := 0
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommand = func(name string, command string, args ...string) error {
		logger.Infof("RUN %d for '%s'. %s %+v", execCount, name, command, args)
		assert.Equal(t, "sgdisk", command)
		switch execCount {
		case 0:
			assert.Equal(t, []string{"--zap-all", "/dev/sda"}, args)
		case 1:
			assert.Equal(t, []string{"--clear", "--mbrtogpt", "/dev/sda"}, args)
		case 2:
			assert.Equal(t, 11, len(args))
			assert.Equal(t, "--change-name=1:ROOK-OSD1-WAL", args[1])
			assert.Equal(t, "--change-name=2:ROOK-OSD1-DB", args[4])
			assert.Equal(t, "--change-name=3:ROOK-OSD1-BLOCK", args[7])
		}
		execCount++
		return nil
	}

	// setup a context with 1 disk: sda
	context := &clusterd.Context{EtcdClient: etcdClient, Executor: executor, NodeID: nodeID, ConfigDir: configDir, Inventory: createInventory()}
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sda", Size: 100},
	}

	// setup a partition scheme for data and metadata to be collocated on sda
	bluestoreConfig := partition.BluestoreConfig{WalSizeMB: 1, DatabaseSizeMB: 2}
	entry := partition.NewPerfSchemeEntry()
	entry.ID = 1
	entry.OsdUUID = uuid.Must(uuid.NewRandom())
	partition.PopulateCollocatedPerfSchemeEntry(entry, "sda", bluestoreConfig)

	config := &osdConfig{configRoot: configDir, rootPath: filepath.Join(configDir, "osd1"), id: entry.ID,
		uuid: entry.OsdUUID, dir: false, partitionScheme: entry}

	// partition the OSD on sda now
	err = partitionBluestoreOSD(context, config)
	assert.Nil(t, err)
	assert.Equal(t, 3, execCount)

	// verify that both the data and metadata have been associated with the device in etcd (since data/metadata are collocated)
	blockDetails, err := getBlockPartitionDetails(config)
	assert.Nil(t, err)
	dataID := etcdClient.GetValue(
		fmt.Sprintf("/rook/services/ceph/osd/desired/node123/device/%s/osd-id-data", blockDetails.DiskUUID))
	assert.Equal(t, "1", dataID)

	metadataID := etcdClient.GetValue(
		fmt.Sprintf("/rook/services/ceph/osd/desired/node123/device/%s/osd-id-metadata", blockDetails.DiskUUID))
	assert.Equal(t, "1", metadataID)
}
