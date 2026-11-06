package mysqlagent

import (
	"context"
	"errors"
	"testing"

	"github.com/fulcrumproject/agent-lib-go/pkg/agent"
	"github.com/fulcrumproject/agent-lib-go/pkg/stdagent"
	"github.com/fulcrumproject/agent-mysql/internal/config"
)

const aid = "11111111-1111-4111-8111-111111111111"
const sid = "22222222-2222-4222-8222-222222222222"
const testPassword = "test-only-long-password"

type fakeCore struct {
	stdagent.FulcrumClient
	secret any
	err    error
}

func (f *fakeCore) GetSecret(string) (any, error) { return f.secret, f.err }

type fakeDocker struct {
	calls  []string
	props  Properties
	cpu    int
	memory int64
	err    error
}

func (f *fakeDocker) Ping(context.Context) error { return nil }
func (f *fakeDocker) Close() error               { return nil }
func (f *fakeDocker) Create(_ context.Context, _ string, p Properties) (*Instance, error) {
	f.calls = append(f.calls, "create")
	f.props = p
	return &Instance{Port: p.Port}, f.err
}
func (f *fakeDocker) Start(context.Context, string) (*Instance, error) {
	f.calls = append(f.calls, "start")
	return &Instance{}, f.err
}
func (f *fakeDocker) Stop(context.Context, string) (*Instance, error) {
	f.calls = append(f.calls, "stop")
	return &Instance{}, f.err
}
func (f *fakeDocker) Update(_ context.Context, _ string, cpu int, memory int64) (*Instance, error) {
	f.calls = append(f.calls, "update")
	f.cpu = cpu
	f.memory = memory
	return &Instance{}, f.err
}
func (f *fakeDocker) Delete(context.Context, string) error {
	f.calls = append(f.calls, "delete")
	return f.err
}
func (f *fakeDocker) Metrics(context.Context, string) (Metrics, error) {
	return Metrics{Available: 1, StorageFreeBytes: 123}, f.err
}

func setup(t *testing.T) (*MySQLAgent, *fakeDocker, *fakeCore, *agent.Service[Properties, Instance]) {
	t.Helper()
	f := &fakeDocker{}
	core := &fakeCore{secret: testPassword}
	a, err := NewMySQLAgent(func(config.DockerConfig, string) (DockerClient, error) { return f, nil }, core)
	if err != nil {
		t.Fatal(err)
	}
	err = a.ConnectHandler(context.Background(), &agent.AgentInfo[RemoteConfig]{ID: aid, Config: &RemoteConfig{Docker: config.DockerConfig{BindIP: "127.0.0.1", Host: "localhost"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := &agent.Service[Properties, Instance]{ID: sid, AgentID: aid, Status: agent.ServiceStatusNew, Properties: &Properties{CPU: 1, Memory: 1024, Database: "app", Username: "appuser", Password: "vault://test-ref", Port: 33061}}
	return a, f, core, s
}

func TestCreateRetryResolvesServiceSecret(t *testing.T) {
	a, f, _, s := setup(t)
	// The wrapper produces a zero-valued Params for explicit retry actions.
	r, err := a.CreateHandler()(context.Background(), &agent.Job[Properties, Properties, Instance]{Service: s, Params: &Properties{}})
	if err != nil {
		t.Fatal(err)
	}
	if f.props.Password != testPassword || *r.AgentInstanceID != sid {
		t.Fatal("secret or stable identity not propagated")
	}
	if s.Properties.Password != "vault://test-ref" {
		t.Fatal("mutated the service's vault reference")
	}
}
func TestCreateFailureIsNotSuccessOrCleanup(t *testing.T) {
	a, f, _, s := setup(t)
	f.err = errors.New("provision failed")
	r, err := a.CreateHandler()(context.Background(), &agent.Job[Properties, Properties, Instance]{Service: s})
	if err == nil || r != nil || len(f.calls) != 1 || f.calls[0] != "create" {
		t.Fatal("failure was hidden or triggered destructive cleanup")
	}
}
func TestSecretResolutionErrorIsRedacted(t *testing.T) {
	a, _, core, s := setup(t)
	core.err = errors.New(testPassword)
	_, err := a.CreateHandler()(context.Background(), &agent.Job[Properties, Properties, Instance]{Service: s})
	if err == nil || err.Error() != "cannot resolve database password" {
		t.Fatal("unexpected secret error")
	}
}
func TestStateAndOwnershipGuards(t *testing.T) {
	a, f, _, s := setup(t)
	j := &agent.Job[struct{}, Properties, Instance]{Service: s}
	if _, err := a.StartHandler()(context.Background(), j); err == nil {
		t.Fatal("start from New allowed")
	}
	if _, err := a.DeleteHandler()(context.Background(), j); err == nil {
		t.Fatal("delete from New allowed")
	}
	s.Status = agent.ServiceStatusStarted
	if _, err := a.DeleteHandler()(context.Background(), j); err == nil {
		t.Fatal("delete while started allowed")
	}
	if _, err := a.UpdateHandler()(context.Background(), &agent.Job[UpdateProperties, Properties, Instance]{Service: s, Params: &UpdateProperties{}}); err == nil {
		t.Fatal("hot update allowed")
	}
	s.AgentID = sid
	if _, err := a.StopHandler()(context.Background(), j); err == nil {
		t.Fatal("foreign service accepted")
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid request reached runtime")
	}
}
func TestStopAllowsPartialCreateCleanup(t *testing.T) {
	a, f, _, s := setup(t)
	if _, err := a.StopHandler()(context.Background(), &agent.Job[struct{}, Properties, Instance]{Service: s}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "stop" {
		t.Fatal("missing stop")
	}
}
func TestPartialUpdatePreservesOtherLimit(t *testing.T) {
	a, f, _, s := setup(t)
	s.Status = agent.ServiceStatusStopped
	cpu := 2
	if _, err := a.UpdateHandler()(context.Background(), &agent.Job[UpdateProperties, Properties, Instance]{Service: s, Params: &UpdateProperties{CPU: &cpu}}); err != nil {
		t.Fatal(err)
	}
	if f.cpu != 2 || f.memory != 1024 {
		t.Fatal("partial update erased existing resource values")
	}
	zero := 0
	if _, err := a.UpdateHandler()(context.Background(), &agent.Job[UpdateProperties, Properties, Instance]{Service: s, Params: &UpdateProperties{CPU: &zero}}); err == nil {
		t.Fatal("zero limit accepted")
	}
}
func TestUpdateRejectsImmutablePropertiesEvenIfCoreAcceptedAReference(t *testing.T) {
	a, f, _, s := setup(t)
	s.Status = agent.ServiceStatusStopped
	changed := "another-long-password"
	_, err := a.UpdateHandler()(context.Background(), &agent.Job[UpdateProperties, Properties, Instance]{Service: s, Params: &UpdateProperties{Password: &changed}})
	if err == nil || len(f.calls) != 0 {
		t.Fatal("credential update reached Docker")
	}
}

func TestMetricsUseStableIdentity(t *testing.T) {
	a, _, _, s := setup(t)
	s.Status = agent.ServiceStatusStarted
	m, err := a.MetricsReporter()(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 {
		t.Fatal("missing metrics")
	}
	for _, v := range m {
		if v.AgentInstanceID != sid || v.ResourceID != sid {
			t.Fatal("metrics identity mismatch")
		}
	}
}
func TestPropertiesRejectUnsafeInputs(t *testing.T) {
	_, _, _, s := setup(t)
	p := *s.Properties
	p.Password = testPassword
	c := config.DockerConfig{}.WithDefaults()
	if err := p.Validate(c); err != nil {
		t.Fatal(err)
	}
	cases := []Properties{p, p, p, p, p, p}
	cases[0].Database = "mysql"
	cases[1].Username = "root"
	cases[2].Database = "a;DROP"
	cases[3].Password = "short"
	cases[4].Port = 0
	cases[5].Memory = 512
	for _, v := range cases {
		if v.Validate(c) == nil {
			t.Fatal("unsafe properties accepted")
		}
	}
}
