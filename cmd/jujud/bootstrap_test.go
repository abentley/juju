// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/names"
	gitjujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	"github.com/juju/utils/set"
	goyaml "gopkg.in/yaml.v1"
	gc "launchpad.net/gocheck"

	"github.com/juju/juju/agent"
	agenttools "github.com/juju/juju/agent/tools"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environmentserver/authentication"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/configstore"
	envtesting "github.com/juju/juju/environs/testing"
	envtools "github.com/juju/juju/environs/tools"
	"github.com/juju/juju/instance"
	jujutesting "github.com/juju/juju/juju/testing"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/dummy"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/api/params"
	"github.com/juju/juju/testing"
	"github.com/juju/juju/tools"
	"github.com/juju/juju/version"
	"github.com/juju/juju/worker/peergrouper"
)

var _ = configstore.Default

// We don't want to use JujuConnSuite because it gives us
// an already-bootstrapped environment.
type BootstrapSuite struct {
	testing.BaseSuite
	gitjujutesting.MgoSuite
	envcfg          *config.Config
	b64yamlEnvcfg   string
	instanceId      instance.Id
	dataDir         string
	logDir          string
	mongoOplogSize  string
	fakeEnsureMongo fakeEnsure
	bootstrapName   string
}

var _ = gc.Suite(&BootstrapSuite{})

type fakeEnsure struct {
	ensureCount    int
	initiateCount  int
	dataDir        string
	namespace      string
	oplogSize      int
	info           params.StateServingInfo
	initiateParams peergrouper.InitiateMongoParams
	err            error
}

func (f *fakeEnsure) fakeEnsureMongo(args mongo.EnsureServerParams) error {
	f.ensureCount++
	f.dataDir, f.namespace, f.info, f.oplogSize = args.DataDir, args.Namespace, args.StateServingInfo, args.OplogSize
	return f.err
}

func (f *fakeEnsure) fakeInitiateMongo(p peergrouper.InitiateMongoParams) error {
	f.initiateCount++
	f.initiateParams = p
	return nil
}

func (s *BootstrapSuite) SetUpSuite(c *gc.C) {
	s.PatchValue(&ensureMongoServer, s.fakeEnsureMongo.fakeEnsureMongo)
	s.PatchValue(&maybeInitiateMongoServer, s.fakeEnsureMongo.fakeInitiateMongo)

	s.BaseSuite.SetUpSuite(c)
	s.MgoSuite.SetUpSuite(c)
	s.makeTestEnv(c)
}

func (s *BootstrapSuite) TearDownSuite(c *gc.C) {
	s.MgoSuite.TearDownSuite(c)
	s.BaseSuite.TearDownSuite(c)
	dummy.Reset()
}

func (s *BootstrapSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.PatchValue(&version.Current.Series, "trusty") // for predictable tools
	s.PatchValue(&sshGenerateKey, func(name string) (string, string, error) {
		return "private-key", "public-key", nil
	})

	s.MgoSuite.SetUpTest(c)
	s.dataDir = c.MkDir()
	s.logDir = c.MkDir()
	s.mongoOplogSize = "1234"
	s.fakeEnsureMongo = fakeEnsure{}

	// Create fake tools.tar.gz and downloaded-tools.txt.
	toolsDir := filepath.FromSlash(agenttools.SharedToolsDir(s.dataDir, version.Current))
	err := os.MkdirAll(toolsDir, 0755)
	c.Assert(err, gc.IsNil)
	err = ioutil.WriteFile(filepath.Join(toolsDir, "tools.tar.gz"), nil, 0644)
	c.Assert(err, gc.IsNil)
	s.writeDownloadedTools(c, &tools.Tools{Version: version.Current})
}

func (s *BootstrapSuite) TearDownTest(c *gc.C) {
	s.MgoSuite.TearDownTest(c)
	s.BaseSuite.TearDownTest(c)
}

func (s *BootstrapSuite) writeDownloadedTools(c *gc.C, tools *tools.Tools) {
	toolsDir := filepath.FromSlash(agenttools.SharedToolsDir(s.dataDir, tools.Version))
	err := os.MkdirAll(toolsDir, 0755)
	c.Assert(err, gc.IsNil)
	data, err := json.Marshal(tools)
	c.Assert(err, gc.IsNil)
	err = ioutil.WriteFile(filepath.Join(toolsDir, "downloaded-tools.txt"), data, 0644)
	c.Assert(err, gc.IsNil)
}

var testPassword = "my-admin-secret"

func testPasswordHash() string {
	return utils.UserPasswordHash(testPassword, utils.CompatSalt)
}

func (s *BootstrapSuite) initBootstrapCommand(c *gc.C, jobs []params.MachineJob, args ...string) (machineConf agent.ConfigSetterWriter, cmd *BootstrapCommand, err error) {
	if len(jobs) == 0 {
		// Add default jobs.
		jobs = []params.MachineJob{
			params.JobManageEnviron, params.JobHostUnits,
		}
	}
	// NOTE: the old test used an equivalent of the NewAgentConfig, but it
	// really should be using NewStateMachineConfig.
	agentParams := agent.AgentConfigParams{
		LogDir:            s.logDir,
		DataDir:           s.dataDir,
		Jobs:              jobs,
		Tag:               names.NewMachineTag("0"),
		UpgradedToVersion: version.Current.Number,
		Password:          testPasswordHash(),
		Nonce:             agent.BootstrapNonce,
		StateAddresses:    []string{gitjujutesting.MgoServer.Addr()},
		APIAddresses:      []string{"0.1.2.3:1234"},
		CACert:            testing.CACert,
		Values: map[string]string{
			agent.Namespace:      "foobar",
			agent.MongoOplogSize: s.mongoOplogSize,
		},
	}
	servingInfo := params.StateServingInfo{
		Cert:       "some cert",
		PrivateKey: "some key",
		APIPort:    3737,
		StatePort:  gitjujutesting.MgoServer.Port(),
	}

	machineConf, err = agent.NewStateMachineConfig(agentParams, servingInfo)
	c.Assert(err, gc.IsNil)
	err = machineConf.Write()
	c.Assert(err, gc.IsNil)

	cmd = &BootstrapCommand{}

	err = testing.InitCommand(cmd, append([]string{"--data-dir", s.dataDir}, args...))
	return machineConf, cmd, err
}

func (s *BootstrapSuite) TestInitializeEnvironment(c *gc.C) {
	hw := instance.MustParseHardware("arch=amd64 mem=8G")
	machConf, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId), "--hardware", hw.String())
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	c.Assert(s.fakeEnsureMongo.dataDir, gc.Equals, s.dataDir)
	c.Assert(s.fakeEnsureMongo.initiateCount, gc.Equals, 1)
	c.Assert(s.fakeEnsureMongo.ensureCount, gc.Equals, 1)
	c.Assert(s.fakeEnsureMongo.dataDir, gc.Equals, s.dataDir)
	c.Assert(s.fakeEnsureMongo.oplogSize, gc.Equals, 1234)

	expectInfo, exists := machConf.StateServingInfo()
	c.Assert(exists, jc.IsTrue)
	c.Assert(expectInfo.SharedSecret, gc.Equals, "")
	c.Assert(expectInfo.SystemIdentity, gc.Equals, "")

	servingInfo := s.fakeEnsureMongo.info
	c.Assert(len(servingInfo.SharedSecret), gc.Not(gc.Equals), 0)
	c.Assert(len(servingInfo.SystemIdentity), gc.Not(gc.Equals), 0)
	servingInfo.SharedSecret = ""
	servingInfo.SystemIdentity = ""
	c.Assert(servingInfo, jc.DeepEquals, expectInfo)
	expectDialAddrs := []string{fmt.Sprintf("127.0.0.1:%d", expectInfo.StatePort)}
	gotDialAddrs := s.fakeEnsureMongo.initiateParams.DialInfo.Addrs
	c.Assert(gotDialAddrs, gc.DeepEquals, expectDialAddrs)

	memberHost := fmt.Sprintf("%s:%d", s.bootstrapName, expectInfo.StatePort)
	c.Assert(s.fakeEnsureMongo.initiateParams.MemberHostPort, gc.Equals, memberHost)
	c.Assert(s.fakeEnsureMongo.initiateParams.User, gc.Equals, "")
	c.Assert(s.fakeEnsureMongo.initiateParams.Password, gc.Equals, "")

	st, err := state.Open(&authentication.MongoInfo{
		Info: mongo.Info{
			Addrs:  []string{gitjujutesting.MgoServer.Addr()},
			CACert: testing.CACert,
		},
		Password: testPasswordHash(),
	}, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()
	machines, err := st.AllMachines()
	c.Assert(err, gc.IsNil)
	c.Assert(machines, gc.HasLen, 1)

	instid, err := machines[0].InstanceId()
	c.Assert(err, gc.IsNil)
	c.Assert(instid, gc.Equals, instance.Id(string(s.instanceId)))

	stateHw, err := machines[0].HardwareCharacteristics()
	c.Assert(err, gc.IsNil)
	c.Assert(stateHw, gc.NotNil)
	c.Assert(*stateHw, gc.DeepEquals, hw)

	cons, err := st.EnvironConstraints()
	c.Assert(err, gc.IsNil)
	c.Assert(&cons, jc.Satisfies, constraints.IsEmpty)

	cfg, err := st.EnvironConfig()
	c.Assert(err, gc.IsNil)
	c.Assert(cfg.AuthorizedKeys(), gc.Equals, s.envcfg.AuthorizedKeys()+"\npublic-key")
}

func (s *BootstrapSuite) TestInitializeEnvironmentInvalidOplogSize(c *gc.C) {
	s.mongoOplogSize = "NaN"
	hw := instance.MustParseHardware("arch=amd64 mem=8G")
	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId), "--hardware", hw.String())
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.ErrorMatches, `invalid oplog size: "NaN"`)
}

func (s *BootstrapSuite) TestSetConstraints(c *gc.C) {
	tcons := constraints.Value{Mem: uint64p(2048), CpuCores: uint64p(2)}
	_, cmd, err := s.initBootstrapCommand(c, nil,
		"--env-config", s.b64yamlEnvcfg,
		"--instance-id", string(s.instanceId),
		"--constraints", tcons.String(),
	)
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	st, err := state.Open(&authentication.MongoInfo{
		Info: mongo.Info{
			Addrs:  []string{gitjujutesting.MgoServer.Addr()},
			CACert: testing.CACert,
		},
		Password: testPasswordHash(),
	}, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()
	cons, err := st.EnvironConstraints()
	c.Assert(err, gc.IsNil)
	c.Assert(cons, gc.DeepEquals, tcons)

	machines, err := st.AllMachines()
	c.Assert(err, gc.IsNil)
	c.Assert(machines, gc.HasLen, 1)
	cons, err = machines[0].Constraints()
	c.Assert(err, gc.IsNil)
	c.Assert(cons, gc.DeepEquals, tcons)
}

func uint64p(v uint64) *uint64 {
	return &v
}

func (s *BootstrapSuite) TestDefaultMachineJobs(c *gc.C) {
	expectedJobs := []state.MachineJob{
		state.JobManageEnviron, state.JobHostUnits,
	}
	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	st, err := state.Open(&authentication.MongoInfo{
		Info: mongo.Info{
			Addrs:  []string{gitjujutesting.MgoServer.Addr()},
			CACert: testing.CACert,
		},
		Password: testPasswordHash(),
	}, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()
	m, err := st.Machine("0")
	c.Assert(err, gc.IsNil)
	c.Assert(m.Jobs(), gc.DeepEquals, expectedJobs)
}

func (s *BootstrapSuite) TestConfiguredMachineJobs(c *gc.C) {
	jobs := []params.MachineJob{params.JobManageEnviron}
	_, cmd, err := s.initBootstrapCommand(c, jobs, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	st, err := state.Open(&authentication.MongoInfo{
		Info: mongo.Info{
			Addrs:  []string{gitjujutesting.MgoServer.Addr()},
			CACert: testing.CACert,
		},
		Password: testPasswordHash(),
	}, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()
	m, err := st.Machine("0")
	c.Assert(err, gc.IsNil)
	c.Assert(m.Jobs(), gc.DeepEquals, []state.MachineJob{state.JobManageEnviron})
}

func testOpenState(c *gc.C, info *authentication.MongoInfo, expectErrType error) {
	st, err := state.Open(info, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	if st != nil {
		st.Close()
	}
	if expectErrType != nil {
		c.Assert(err, gc.FitsTypeOf, expectErrType)
	} else {
		c.Assert(err, gc.IsNil)
	}
}

func (s *BootstrapSuite) TestInitialPassword(c *gc.C) {
	machineConf, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)

	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	info := &authentication.MongoInfo{
		Info: mongo.Info{
			Addrs:  []string{gitjujutesting.MgoServer.Addr()},
			CACert: testing.CACert,
		},
	}

	// Check we can log in to mongo as admin.
	// TODO(dfc) does passing nil for the admin user name make your skin crawl ? mine too.
	info.Tag, info.Password = nil, testPasswordHash()
	st, err := state.Open(info, mongo.DefaultDialOpts(), environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()

	// We're running Mongo with --noauth; let's explicitly verify
	// that we can login as that user. Even with --noauth, an
	// explicit Login will still be verified.
	adminDB := st.MongoSession().DB("admin")
	err = adminDB.Login("admin", "invalid-password")
	c.Assert(err, gc.ErrorMatches, "auth fail(s|ed)")
	err = adminDB.Login("admin", info.Password)
	c.Assert(err, gc.IsNil)

	// Check that the admin user has been given an appropriate
	// password
	u, err := st.User("admin")
	c.Assert(err, gc.IsNil)
	c.Assert(u.PasswordValid(testPassword), gc.Equals, true)

	// Check that the machine configuration has been given a new
	// password and that we can connect to mongo as that machine
	// and that the in-mongo password also verifies correctly.
	machineConf1, err := agent.ReadConfig(agent.ConfigPath(machineConf.DataDir(), names.NewMachineTag("0")))
	c.Assert(err, gc.IsNil)

	stateinfo, ok := machineConf1.MongoInfo()
	c.Assert(ok, jc.IsTrue)
	st, err = state.Open(stateinfo, mongo.DialOpts{}, environs.NewStatePolicy())
	c.Assert(err, gc.IsNil)
	defer st.Close()

	m, err := st.Machine("0")
	c.Assert(err, gc.IsNil)
	c.Assert(m.HasVote(), jc.IsTrue)
}

var bootstrapArgTests = []struct {
	input              []string
	err                string
	expectedInstanceId string
	expectedHardware   instance.HardwareCharacteristics
	expectedConfig     map[string]interface{}
}{
	{
		// no value supplied for env-config
		err: "--env-config option must be set",
	}, {
		// empty env-config
		input: []string{"--env-config", ""},
		err:   "--env-config option must be set",
	}, {
		// wrong, should be base64
		input: []string{"--env-config", "name: banana\n"},
		err:   ".*illegal base64 data at input byte.*",
	}, {
		// no value supplied for instance-id
		input: []string{
			"--env-config", base64.StdEncoding.EncodeToString([]byte("name: banana\n")),
		},
		err: "--instance-id option must be set",
	}, {
		// empty instance-id
		input: []string{
			"--env-config", base64.StdEncoding.EncodeToString([]byte("name: banana\n")),
			"--instance-id", "",
		},
		err: "--instance-id option must be set",
	}, {
		input: []string{
			"--env-config", base64.StdEncoding.EncodeToString([]byte("name: banana\n")),
			"--instance-id", "anything",
		},
		expectedInstanceId: "anything",
		expectedConfig:     map[string]interface{}{"name": "banana"},
	}, {
		input: []string{
			"--env-config", base64.StdEncoding.EncodeToString([]byte("name: banana\n")),
			"--instance-id", "anything",
			"--hardware", "nonsense",
		},
		err: `invalid value "nonsense" for flag --hardware: malformed characteristic "nonsense"`,
	}, {
		input: []string{
			"--env-config", base64.StdEncoding.EncodeToString([]byte("name: banana\n")),
			"--instance-id", "anything",
			"--hardware", "arch=amd64 cpu-cores=4 root-disk=2T",
		},
		expectedInstanceId: "anything",
		expectedHardware:   instance.MustParseHardware("arch=amd64 cpu-cores=4 root-disk=2T"),
		expectedConfig:     map[string]interface{}{"name": "banana"},
	},
}

func (s *BootstrapSuite) TestBootstrapArgs(c *gc.C) {
	for i, t := range bootstrapArgTests {
		c.Logf("test %d", i)
		var args []string
		args = append(args, t.input...)
		_, cmd, err := s.initBootstrapCommand(c, nil, args...)
		if t.err == "" {
			c.Assert(cmd, gc.NotNil)
			c.Assert(err, gc.IsNil)
			c.Assert(cmd.EnvConfig, gc.DeepEquals, t.expectedConfig)
			c.Assert(cmd.InstanceId, gc.Equals, t.expectedInstanceId)
			c.Assert(cmd.Hardware, gc.DeepEquals, t.expectedHardware)
		} else {
			c.Assert(err, gc.ErrorMatches, t.err)
		}
	}
}

func (s *BootstrapSuite) TestInitializeStateArgs(c *gc.C) {
	var called int
	initializeState := func(_ agent.ConfigSetter, envCfg *config.Config, machineCfg agent.BootstrapMachineConfig, dialOpts mongo.DialOpts, policy state.Policy) (_ *state.State, _ *state.Machine, resultErr error) {
		called++
		c.Assert(dialOpts.Direct, gc.Equals, true)
		c.Assert(dialOpts.Timeout, gc.Equals, 30*time.Second)
		c.Assert(dialOpts.SocketTimeout, gc.Equals, 123*time.Second)
		return nil, nil, errors.New("failed to initialize state")
	}
	s.PatchValue(&agentInitializeState, initializeState)
	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.ErrorMatches, "failed to initialize state")
	c.Assert(called, gc.Equals, 1)
}

func (s *BootstrapSuite) TestInitializeStateMinSocketTimeout(c *gc.C) {
	var called int
	initializeState := func(_ agent.ConfigSetter, envCfg *config.Config, machineCfg agent.BootstrapMachineConfig, dialOpts mongo.DialOpts, policy state.Policy) (_ *state.State, _ *state.Machine, resultErr error) {
		called++
		c.Assert(dialOpts.Direct, gc.Equals, true)
		c.Assert(dialOpts.SocketTimeout, gc.Equals, 1*time.Minute)
		return nil, nil, errors.New("failed to initialize state")
	}

	envcfg, err := s.envcfg.Apply(map[string]interface{}{
		"bootstrap-timeout": "13",
	})
	c.Assert(err, gc.IsNil)
	b64yamlEnvcfg := b64yaml(envcfg.AllAttrs()).encode()

	s.PatchValue(&agentInitializeState, initializeState)
	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.ErrorMatches, "failed to initialize state")
	c.Assert(called, gc.Equals, 1)
}

func (s *BootstrapSuite) TestSystemIdentityWritten(c *gc.C) {
	_, err := os.Stat(filepath.Join(s.dataDir, agent.SystemIdentity))
	c.Assert(err, jc.Satisfies, os.IsNotExist)

	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	data, err := ioutil.ReadFile(filepath.Join(s.dataDir, agent.SystemIdentity))
	c.Assert(err, gc.IsNil)
	c.Assert(string(data), gc.Equals, "private-key")
}

func (s *BootstrapSuite) TestDownloadedToolsMetadata(c *gc.C) {
	// Tools downloaded by cloud-init script.
	s.testToolsMetadata(c, false)
}

func (s *BootstrapSuite) TestUploadedToolsMetadata(c *gc.C) {
	// Tools uploaded over ssh.
	s.writeDownloadedTools(c, &tools.Tools{
		Version: version.Current,
		URL:     "file:///does/not/matter",
	})
	s.testToolsMetadata(c, true)
}

func (s *BootstrapSuite) testToolsMetadata(c *gc.C, exploded bool) {
	provider, err := environs.Provider(s.envcfg.Type())
	c.Assert(err, gc.IsNil)
	env, err := provider.Open(s.envcfg)
	c.Assert(err, gc.IsNil)
	oldMetadata, err := envtools.ReadMetadata(env.Storage())
	c.Assert(err, gc.IsNil)

	_, cmd, err := s.initBootstrapCommand(c, nil, "--env-config", s.b64yamlEnvcfg, "--instance-id", string(s.instanceId))
	c.Assert(err, gc.IsNil)
	err = cmd.Run(nil)
	c.Assert(err, gc.IsNil)

	newMetadata, err := envtools.ReadMetadata(env.Storage())
	c.Assert(err, gc.IsNil)
	if !exploded {
		c.Assert(newMetadata, gc.HasLen, len(oldMetadata))
	} else {
		// new metadata should have more tools.
		c.Assert(len(newMetadata), jc.GreaterThan, len(oldMetadata))
		var expectedSeries set.Strings
		for _, series := range version.SupportedSeries() {
			os, err := version.GetOSFromSeries(series)
			c.Assert(err, gc.IsNil)
			if os == version.Ubuntu {
				expectedSeries.Add(series)
			}
		}
		c.Assert(newMetadata, gc.HasLen, expectedSeries.Size())
		for _, m := range newMetadata {
			c.Assert(expectedSeries.Contains(m.Release), jc.IsTrue)
		}
	}
}

func (s *BootstrapSuite) makeTestEnv(c *gc.C) {
	attrs := dummy.SampleConfig().Merge(
		testing.Attrs{
			"agent-version":     version.Current.Number.String(),
			"bootstrap-timeout": "123",
		},
	).Delete("admin-secret", "ca-private-key")

	cfg, err := config.New(config.NoDefaults, attrs)
	c.Assert(err, gc.IsNil)
	provider, err := environs.Provider(cfg.Type())
	c.Assert(err, gc.IsNil)
	env, err := provider.Prepare(nullContext(), cfg)
	c.Assert(err, gc.IsNil)

	envtesting.MustUploadFakeTools(env.Storage())
	inst, _, _, err := jujutesting.StartInstance(env, "0")
	c.Assert(err, gc.IsNil)
	s.instanceId = inst.Id()

	addresses, err := inst.Addresses()
	c.Assert(err, gc.IsNil)
	s.bootstrapName = network.SelectPublicAddress(addresses)
	s.envcfg = env.Config()
	s.b64yamlEnvcfg = b64yaml(s.envcfg.AllAttrs()).encode()
}

func nullContext() *cmd.Context {
	ctx, _ := cmd.DefaultContext()
	ctx.Stdin = io.LimitReader(nil, 0)
	ctx.Stdout = ioutil.Discard
	ctx.Stderr = ioutil.Discard
	return ctx
}

type b64yaml map[string]interface{}

func (m b64yaml) encode() string {
	data, err := goyaml.Marshal(m)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}
