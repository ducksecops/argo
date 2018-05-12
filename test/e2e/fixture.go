package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/cmd/argocd/commands"
	"github.com/argoproj/argo-cd/common"
	"github.com/argoproj/argo-cd/controller"
	"github.com/argoproj/argo-cd/install"
	"github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/reposerver"
	"github.com/argoproj/argo-cd/reposerver/repository"
	"github.com/argoproj/argo-cd/server"
	"github.com/argoproj/argo-cd/server/cluster"
	apirepository "github.com/argoproj/argo-cd/server/repository"
	"github.com/argoproj/argo-cd/util"
	"github.com/argoproj/argo-cd/util/cache"
	"github.com/argoproj/argo-cd/util/git"
	"github.com/argoproj/argo-cd/util/settings"
	"k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	TestTimeout = time.Minute * 3
)

// Fixture represents e2e tests fixture.
type Fixture struct {
	Config            *rest.Config
	KubeClient        kubernetes.Interface
	ExtensionsClient  apiextensionsclient.Interface
	AppClient         appclientset.Interface
	Namespace         string
	InstanceID        string
	RepoServerAddress string
	ApiServerAddress  string

	tearDownCallback func()
}

func createNamespace(kubeClient *kubernetes.Clientset) (string, error) {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "argo-e2e-test-",
		},
	}
	cns, err := kubeClient.CoreV1().Namespaces().Create(ns)
	if err != nil {
		return "", err
	}
	return cns.Name, nil
}

func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer util.Close(l)
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (f *Fixture) setup() error {
	installer, err := install.NewInstaller(f.Config, install.InstallOptions{})
	if err != nil {
		return err
	}
	installer.InstallApplicationCRD()

	settingsMgr := settings.NewSettingsManager(f.KubeClient, f.Namespace)
	err = settingsMgr.SaveSettings(&settings.ArgoCDSettings{})
	if err != nil {
		return err
	}

	err = f.ensureClusterRegistered()
	if err != nil {
		return err
	}

	apiServerPort, err := getFreePort()
	if err != nil {
		return err
	}

	memCache := cache.NewInMemoryCache(repository.DefaultRepoCacheExpiration)
	repoServerGRPC := reposerver.NewServer(&FakeGitClientFactory{}, memCache).CreateGRPC()
	repoServerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	f.RepoServerAddress = repoServerListener.Addr().String()
	f.ApiServerAddress = fmt.Sprintf("127.0.0.1:%d", apiServerPort)

	apiServer := server.NewServer(server.ArgoCDServerOpts{
		Namespace:     f.Namespace,
		AppClientset:  f.AppClient,
		DisableAuth:   true,
		Insecure:      true,
		KubeClientset: f.KubeClient,
		RepoClientset: reposerver.NewRepositoryServerClientset(f.RepoServerAddress),
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		err = repoServerGRPC.Serve(repoServerListener)
	}()
	go func() {
		apiServer.Run(ctx, apiServerPort)
	}()

	f.tearDownCallback = func() {
		cancel()
		repoServerGRPC.Stop()
	}
	return err
}

func (f *Fixture) ensureClusterRegistered() error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	overrides := clientcmd.ConfigOverrides{}
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, &overrides, os.Stdin)
	conf, err := clientConfig.ClientConfig()
	if err != nil {
		return err
	}
	// Install RBAC resources for managing the cluster
	managerBearerToken := common.InstallClusterManagerRBAC(conf)
	clst := commands.NewCluster(f.Config.Host, conf, managerBearerToken)
	_, err = cluster.NewServer(f.Namespace, f.KubeClient, f.AppClient).Create(context.Background(), clst)
	return err
}

// TearDown deletes fixture resources.
func (f *Fixture) TearDown() {
	if f.tearDownCallback != nil {
		f.tearDownCallback()
	}
	err := f.KubeClient.CoreV1().Namespaces().Delete(f.Namespace, &metav1.DeleteOptions{})
	if err != nil {
		println("Unable to tear down fixture")
	}
}

// GetKubeConfig creates new kubernetes client config using specified config path and config overrides variables
func GetKubeConfig(configPath string, overrides clientcmd.ConfigOverrides) *rest.Config {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = configPath
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, &overrides, os.Stdin)

	var err error
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	return restConfig
}

// NewFixture creates e2e tests fixture: ensures that Application CRD is installed, creates temporal namespace, starts repo and api server,
// configure currently available cluster.
func NewFixture() (*Fixture, error) {
	config := GetKubeConfig("", clientcmd.ConfigOverrides{})
	extensionsClient := apiextensionsclient.NewForConfigOrDie(config)
	appClient := appclientset.NewForConfigOrDie(config)
	kubeClient := kubernetes.NewForConfigOrDie(config)
	namespace, err := createNamespace(kubeClient)
	if err != nil {
		return nil, err
	}

	fixture := &Fixture{
		Config:           config,
		ExtensionsClient: extensionsClient,
		AppClient:        appClient,
		KubeClient:       kubeClient,
		Namespace:        namespace,
		InstanceID:       namespace,
	}
	err = fixture.setup()
	if err != nil {
		return nil, err
	}
	return fixture, nil
}

// CreateApp creates application with appropriate controller instance id.
func (f *Fixture) CreateApp(t *testing.T, application *v1alpha1.Application) *v1alpha1.Application {
	labels := application.ObjectMeta.Labels
	if labels == nil {
		labels = make(map[string]string)
		application.ObjectMeta.Labels = labels
	}
	labels[common.LabelKeyApplicationControllerInstanceID] = f.InstanceID

	app, err := f.AppClient.ArgoprojV1alpha1().Applications(f.Namespace).Create(application)
	if err != nil {
		t.Fatal(fmt.Sprintf("Unable to create app %v", err))
	}
	return app
}

// CreateController creates new controller instance
func (f *Fixture) CreateController() *controller.ApplicationController {
	repoService := apirepository.NewServer(f.Namespace, f.KubeClient, f.AppClient)
	clusterService := cluster.NewServer(f.Namespace, f.KubeClient, f.AppClient)
	appStateManager := controller.NewAppStateManager(
		clusterService, repoService, f.AppClient, reposerver.NewRepositoryServerClientset(f.RepoServerAddress), f.Namespace)

	appHealthManager := controller.NewAppHealthManager(clusterService, f.Namespace)

	return controller.NewApplicationController(
		f.Namespace,
		f.KubeClient,
		f.AppClient,
		cluster.NewServer(f.Namespace, f.KubeClient, f.AppClient),
		appStateManager,
		appHealthManager,
		10*time.Second,
		&controller.ApplicationControllerConfig{Namespace: f.Namespace, InstanceID: f.InstanceID})
}

func (f *Fixture) RunCli(args ...string) (string, error) {
	cmd := commands.NewCommand()
	cmd.SetArgs(append(args, "--server", f.ApiServerAddress, "--plaintext"))
	output := new(bytes.Buffer)
	cmd.SetOutput(output)
	err := cmd.Execute()
	return output.String(), err
}

// WaitUntil periodically executes specified condition until it returns true.
func WaitUntil(t *testing.T, condition wait.ConditionFunc) {
	stop := make(chan struct{})
	isClosed := false
	makeSureClosed := func() {
		if !isClosed {
			close(stop)
			isClosed = true
		}
	}
	defer makeSureClosed()
	go func() {
		time.Sleep(TestTimeout)
		makeSureClosed()
	}()
	err := wait.PollUntil(time.Second, condition, stop)
	if err != nil {
		t.Fatal("Failed to wait for expected condition")
	}
}

type FakeGitClientFactory struct{}

func (f *FakeGitClientFactory) NewClient(repoURL, path, username, password, sshPrivateKey string) git.Client {
	return &FakeGitClient{
		root: path,
	}
}

// FakeGitClient is a test git client implementation which always clone local test repo.
type FakeGitClient struct {
	root string
}

func (c *FakeGitClient) Init() error {
	_, err := exec.Command("rm", "-rf", c.root).Output()
	if err != nil {
		return err
	}
	_, err = exec.Command("cp", "-r", "../../examples/guestbook", c.root).Output()
	return err
}

func (c *FakeGitClient) Root() string {
	return c.root
}

func (c *FakeGitClient) Fetch() error {
	// do nothing
	return nil
}

func (c *FakeGitClient) Checkout(revision string) error {
	// do nothing
	return nil
}

func (c *FakeGitClient) Reset() error {
	// do nothing
	return nil
}

func (c *FakeGitClient) LsRemote(s string) (string, error) {
	return "abcdef123456890", nil
}

func (c *FakeGitClient) CommitSHA() (string, error) {
	return "abcdef123456890", nil
}