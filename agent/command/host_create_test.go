package command

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/agent/internal"
	"github.com/evergreen-ci/evergreen/agent/internal/client"
	"github.com/evergreen-ci/evergreen/apimodels"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/evergreen-ci/utility"
	"github.com/stretchr/testify/suite"
)

const (
	userdataFileName = "TestPopulateUserdata.sh"
)

type createHostSuite struct {
	params map[string]interface{}
	cmd    createHost
	conf   *internal.TaskConfig
	comm   client.Communicator
	logger client.LoggerProducer

	suite.Suite
}

func TestCreateHostSuite(t *testing.T) {
	suite.Run(t, &createHostSuite{})
}

func (s *createHostSuite) SetupSuite() {
	var err error
	s.comm = client.NewMock("http://localhost.com")
	s.conf = &internal.TaskConfig{
		Expansions: &util.Expansions{"subnet_id": "subnet-123456"},
		Task:       &task.Task{Id: "mock_id", Secret: "mock_secret"},
		Project:    &model.Project{}}
	s.logger, err = s.comm.GetLoggerProducer(context.Background(), client.TaskData{ID: s.conf.Task.Id, Secret: s.conf.Task.Secret}, nil)
	s.NoError(err)
}

func (s *createHostSuite) SetupTest() {
	s.params = map[string]interface{}{
		"distro":    "myDistro",
		"scope":     "task",
		"subnet_id": "${subnet_id}",
	}
	s.cmd = createHost{}
}

func (s *createHostSuite) TestParamDefaults() {
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))
	s.Equal(apimodels.ProviderEC2, s.cmd.CreateHost.CloudProvider)
	s.Equal(apimodels.DefaultSetupTimeoutSecs, s.cmd.CreateHost.SetupTimeoutSecs)
	s.Equal(apimodels.DefaultTeardownTimeoutSecs, s.cmd.CreateHost.TeardownTimeoutSecs)

	s.params["provider"] = apimodels.ProviderDocker
	s.params["image"] = "my-image"
	s.params["command"] = "echo hi"
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))
	s.True(s.cmd.CreateHost.Background)
	s.Equal(apimodels.DefaultContainerWaitTimeoutSecs, s.cmd.CreateHost.ContainerWaitTimeoutSecs)
}

func (s *createHostSuite) TestParseFromFile() {
	//file for testing parsing from a json file
	tmpdir := s.T().TempDir()
	ebsDevice := []map[string]interface{}{
		map[string]interface{}{
			"device_name": "myDevice",
			"ebs_size":    1,
		},
	}
	path := filepath.Join(tmpdir, "example.json")
	fileContent := map[string]interface{}{
		"distro":           "myDistro",
		"scope":            "task",
		"subnet_id":        "${subnet_id}",
		"ebs_block_device": ebsDevice,
	}
	//parse from JSON file
	s.NoError(utility.WriteJSONFile(path, fileContent))
	_, err := os.Stat(path)
	s.Require().False(os.IsNotExist(err))
	s.params = map[string]interface{}{
		"file": path,
	}

	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))
	s.True(s.cmd.CreateHost.Background)
	s.Equal("myDistro", s.cmd.CreateHost.Distro)
	s.Equal("task", s.cmd.CreateHost.Scope)
	s.Equal("subnet-123456", s.cmd.CreateHost.Subnet)
	s.Equal("myDevice", s.cmd.CreateHost.EBSDevices[0].DeviceName)

	//parse from YAML file
	path = filepath.Join(tmpdir, "example.yml")
	s.NoError(utility.WriteYAMLFile(path, fileContent))
	_, err = os.Stat(path)
	s.Require().False(os.IsNotExist(err))
	s.params = map[string]interface{}{
		"file": path,
	}

	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))
	s.True(s.cmd.CreateHost.Background)
	s.Equal("myDistro", s.cmd.CreateHost.Distro)
	s.Equal("task", s.cmd.CreateHost.Scope)
	s.Equal("subnet-123456", s.cmd.CreateHost.Subnet)
	s.Equal("myDevice", s.cmd.CreateHost.EBSDevices[0].DeviceName)

	//test with both file and other params
	s.params = map[string]interface{}{
		"file":   path,
		"distro": "myDistro",
	}

	err = s.cmd.ParseParams(s.params)
	s.Contains(err.Error(), "no params should be defined when using a file to parse params")

}

func (s *createHostSuite) TestParamValidation() {
	// having no ami or distro is an error
	s.params["distro"] = ""
	s.NoError(s.cmd.ParseParams(s.params))
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "must set exactly one of ami or distro")

	// verify errors if missing required info for ami
	s.params["ami"] = "ami"
	s.NoError(s.cmd.ParseParams(s.params))
	err := s.cmd.expandAndValidate(s.conf)
	s.Contains(err.Error(), "instance_type must be set if ami is set")
	s.Contains(err.Error(), "must specify security_group_ids if ami is set")
	s.Contains(err.Error(), "instance_type must be set if ami is set")

	// valid AMI info
	s.params["instance_type"] = "instance"
	s.params["security_group_ids"] = []string{"foo"}
	s.params["subnet_id"] = "subnet"
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))

	// having a key id but nothing else is an error
	s.params["aws_access_key_id"] = "keyid"
	s.NoError(s.cmd.ParseParams(s.params))
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "aws_access_key_id, aws_secret_access_key, key_name must all be set or unset")
	s.params["aws_secret_access_key"] = "secret"
	s.params["key_name"] = "key"
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))

	// verify errors for things controlled by the agent
	s.params["num_hosts"] = "11"
	s.NoError(s.cmd.ParseParams(s.params))
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "num_hosts must be between 1 and 10")
	s.params["scope"] = "idk"
	s.NoError(s.cmd.ParseParams(s.params))
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "scope must be build or task")
	s.params["timeout_teardown_secs"] = 55
	s.NoError(s.cmd.ParseParams(s.params))
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "timeout_teardown_secs must be between 60 and 604800")

	// Validate num_hosts can be an int
	s.params["timeout_teardown_secs"] = 60
	s.params["scope"] = "task"
	s.params["num_hosts"] = 2
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))

	// Validate docker requirements
	s.params["provider"] = apimodels.ProviderDocker
	s.NoError(s.cmd.ParseParams(s.params))
	s.params["distro"] = ""
	settings, err := evergreen.GetConfig()
	s.NoError(err)
	settings.Providers.Docker.DefaultDistro = "my-default-distro"
	s.NoError(evergreen.UpdateConfig(settings))

	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "docker image must be set")
	s.Contains(s.cmd.expandAndValidate(s.conf).Error(), "num_hosts cannot be greater than 1")
	s.params["image"] = "my-image"
	s.params["command"] = "echo hi"
	s.params["num_hosts"] = 1
	s.params["distro"] = "my-default-distro"
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.expandAndValidate(s.conf))
}

func (s *createHostSuite) TestPopulateUserdata() {
	defer os.RemoveAll(userdataFileName)
	userdataFile := []byte("#!/bin/bash\nsome commands")
	s.NoError(ioutil.WriteFile(userdataFileName, userdataFile, 0644))
	s.cmd.CreateHost = &apimodels.CreateHost{UserdataFile: userdataFileName}
	s.NoError(s.cmd.populateUserdata())
	s.Equal("#!/bin/bash\nsome commands", s.cmd.CreateHost.UserdataCommand)
}

func (s *createHostSuite) TestExecuteCommand() {
	s.NoError(s.cmd.ParseParams(s.params))
	s.NoError(s.cmd.Execute(context.Background(), s.comm, s.logger, s.conf))
	s.Equal("subnet-123456", s.comm.(*client.Mock).CreatedHost.Subnet)
}
