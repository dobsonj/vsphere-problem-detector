package operator

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/govmomi/vapi/rest"
	vapitags "github.com/vmware/govmomi/vapi/tags"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vsphere-problem-detector/pkg/check"
	"github.com/openshift/vsphere-problem-detector/pkg/util"
	"github.com/openshift/vsphere-problem-detector/pkg/version"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/legacy-cloud-providers/vsphere"

	vsphereconfig "k8s.io/cloud-provider-vsphere/pkg/common/config"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vsphere-problem-detector/pkg/check"
	"github.com/openshift/vsphere-problem-detector/pkg/util"
	"github.com/openshift/vsphere-problem-detector/pkg/version"
)

type vSphereCheckerInterface interface {
	runChecks(context.Context, *util.ClusterInfo) (*ResultCollector, error)
}

type vSphereChecker struct {
	controller *vSphereProblemDetectorController
}

var _ vSphereCheckerInterface = &vSphereChecker{}

func newVSphereChecker(c *vSphereProblemDetectorController) vSphereCheckerInterface {
	return &vSphereChecker{controller: c}
}

func (v *vSphereChecker) runChecks(ctx context.Context, clusterInfo *util.ClusterInfo) (*ResultCollector, error) {

	v.controller.metricsCollector.StartMetricCollection()

	resultCollector := NewResultsCollector()

	checkContext, err := v.connect(ctx)
	if err != nil {
		return resultCollector, err
	}

	defer func() {
		if err := checkContext.GovmomiClient.Logout(ctx); err != nil {
			klog.Errorf("Failed to logout: %v", err)
		}
	}()

	// Get the fully-qualified vsphere username
	sessionMgr := session.NewManager(checkContext.VMClient)
	user, err := sessionMgr.UserSession(ctx)
	if err != nil {
		return resultCollector, err
	}

	authManager := object.NewAuthorizationManager(checkContext.VMClient)

	checkContext := &check.CheckContext{
		Context:     ctx,
		AuthManager: authManager,
		VMConfig:    vmConfig,
		VMClient:    vmClient.Client,
		TagManager:  vapitags.NewManager(restClient),
		Username:    user.UserName,
		KubeClient:  v.controller,
		ClusterInfo: clusterInfo,
		// Each check run gets its own cache
		Cache:            check.NewCheckCache(vmClient.Client),
		MetricsCollector: v.controller.metricsCollector,
	}
	infra, err := v.controller.GetInfrastructure(ctx)
	if err != nil {
		return nil, err
	}
	checkContext.Context = ctx
	checkContext.AuthManager = authManager
	checkContext.Username = user.UserName
	checkContext.KubeClient = v.controller
	checkContext.ClusterInfo = clusterInfo

	convertToPlatformSpec(infra, checkContext)

	checkRunner := NewCheckThreadPool(parallelVSPhereCalls, channelBufferSize)

	v.enqueueClusterChecks(checkContext, checkRunner, resultCollector)
	if err := v.enqueueNodeChecks(checkContext, checkRunner, resultCollector); err != nil {
		v.controller.metricsCollector.FinishedAllChecks()
		return resultCollector, err
	}

	klog.V(4).Infof("Waiting for all checks")
	if err := checkRunner.Wait(ctx); err != nil {
		klog.Errorf("error waiting for metrics checks to finish: %v", err)
		v.controller.metricsCollector.FinishedAllChecks()
		return resultCollector, err
	}
	v.finishNodeChecks(checkContext)
	klog.Infof("Finished running all vSphere specific checks in the cluster")
	v.controller.metricsCollector.FinishedAllChecks()
	return resultCollector, nil
}

// The idea here is to create a single source of truth in
// regards to the topology of vSphere infra.
// The API Platform Spec seems like a good place to
// shove this data into.
func convertToPlatformSpec(infra *ocpv1.Infrastructure, checkContext *check.CheckContext) {
	checkContext.PlatformSpec = &ocpv1.VSpherePlatformSpec{}

	if infra.Spec.PlatformSpec.VSphere != nil {
		infra.Spec.PlatformSpec.VSphere.DeepCopyInto(checkContext.PlatformSpec)
	}

	if checkContext.VMConfig != nil {
		config := checkContext.VMConfig
		if checkContext.PlatformSpec != nil {
			if len(checkContext.PlatformSpec.VCenters) != 0 {
				// we need to check if we really need to add to VCenters and FailureDomains
				vcenter := vCentersToMap(checkContext.PlatformSpec.VCenters)

				// vcenter is missing from the map, add it...
				if _, ok := vcenter[config.Workspace.VCenterIP]; !ok {
					convertIntreeToPlatformSpec(config, checkContext.PlatformSpec)
				}
			} else {
				convertIntreeToPlatformSpec(config, checkContext.PlatformSpec)
			}
		}
	}
}

func convertIntreeToPlatformSpec(config *vsphere.VSphereConfig, platformSpec *ocpv1.VSpherePlatformSpec) {
	if ccmVcenter, ok := config.VirtualCenter[config.Workspace.VCenterIP]; ok {
		datacenters := strings.Split(ccmVcenter.Datacenters, ",")

		platformSpec.VCenters = append(platformSpec.VCenters, ocpv1.VSpherePlatformVCenterSpec{
			Server:      config.Workspace.VCenterIP,
			Datacenters: datacenters,
		})

		platformSpec.FailureDomains = append(platformSpec.FailureDomains, ocpv1.VSpherePlatformFailureDomainSpec{
			Name:   "",
			Region: "",
			Zone:   "",
			Server: config.Workspace.VCenterIP,
			Topology: ocpv1.VSpherePlatformTopology{
				Datacenter:   config.Workspace.Datacenter,
				Folder:       config.Workspace.Folder,
				ResourcePool: config.Workspace.ResourcePoolPath,
				Datastore:    config.Workspace.DefaultDatastore,
			},
		})
	}
}

func vCentersToMap(vcenters []ocpv1.VSpherePlatformVCenterSpec) map[string]ocpv1.VSpherePlatformVCenterSpec {
	vcenterMap := make(map[string]ocpv1.VSpherePlatformVCenterSpec, len(vcenters))
	for _, v := range vcenters {
		vcenterMap[v.Server] = v
	}
	return vcenterMap
}

func (c *vSphereChecker) connect(ctx context.Context) (*check.CheckContext, error) {

	// todo: jcallen: blow this up...

	// use api infra as the basis of
	// variables instead of intree
	// external won't have these values...

	cfgString, err := c.getVSphereConfig(ctx)
	if err != nil {
		return nil, err
	}

	// external CCM configuration INI and yaml parser
	// and backward compatible with intree
	externalCfg, err := vsphereconfig.ReadConfig([]byte(cfgString))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %s", err)
	}

	// intree configuration
	cfg, err := parseConfig(cfgString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %s", err)
	}

	username, password, err := c.getCredentials(cfg.Workspace.VCenterIP)
	if err != nil {
		return nil, err
	}

	vmClient, restClient, err := newClient(ctx, cfg, username, password)
	if err != nil {
		if strings.Index(username, "\n") != -1 {
			syncErrrorMetric.WithLabelValues("UsernameWithNewLine").Set(1)
			return nil, fmt.Errorf("failed to connect to %s: username in credentials contains new line", cfg.Workspace.VCenterIP)
		} else {
			syncErrrorMetric.WithLabelValues("UsernameWithNewLine").Set(0)
		}

		if strings.Index(password, "\n") != -1 {
			syncErrrorMetric.WithLabelValues("PasswordWithNewLine").Set(1)
			return nil, fmt.Errorf("failed to connect to %s: password in credentials contains new line", cfg.Workspace.VCenterIP)
		} else {
			syncErrrorMetric.WithLabelValues("PasswordWithNewLine").Set(0)
		}
		syncErrrorMetric.WithLabelValues("InvalidCredentials").Set(1)
		return nil, fmt.Errorf("failed to connect to %s: %s", cfg.Workspace.VCenterIP, err)
	} else {
		syncErrrorMetric.WithLabelValues("InvalidCredentials").Set(0)
	}
	if _, ok := cfg.VirtualCenter[cfg.Workspace.VCenterIP]; ok {
		cfg.VirtualCenter[cfg.Workspace.VCenterIP].User = username
	}

	if strings.Index(username, "@") < 0 {
		klog.Warningf("vCenter username for %s is without domain, please consider using username with full domain name", cfg.Workspace.VCenterIP)
	}

	klog.V(2).Infof("Connected to %s as %s", cfg.Workspace.VCenterIP, username)

	checkContext := &check.CheckContext{
		Context:          ctx,
		VMConfig:         cfg,
		ExternalVMConfig: externalCfg,
		GovmomiClient:    vmClient,
		VMClient:         vmClient.Client,
		TagManager:       vapitags.NewManager(restClient),
	}

	return checkContext, nil
}

func (c *vSphereChecker) getCredentials(vCenterIP string) (string, string, error) {
	secret, err := c.controller.secretLister.Secrets(operatorNamespace).Get(cloudCredentialsSecretName)
	if err != nil {
		return "", "", err
	}
	userKey := vCenterIP + "." + "username"
	username, ok := secret.Data[userKey]
	if !ok {
		return "", "", fmt.Errorf("error parsing secret %q: key %q not found", cloudCredentialsSecretName, userKey)
	}
	passwordKey := vCenterIP + "." + "password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", fmt.Errorf("error parsing secret %q: key %q not found", cloudCredentialsSecretName, passwordKey)
	}

	return string(username), string(password), nil
}

func (c *vSphereChecker) getVSphereConfig(ctx context.Context) (string, error) {
	infra, err := c.controller.infraLister.Get(infrastructureName)
	if err != nil {
		return "", err
	}
	if infra.Status.PlatformStatus == nil {
		return "", fmt.Errorf("unknown platform: infrastructure status.platformStatus is nil")
	}
	if infra.Status.PlatformStatus.Type != ocpv1.VSpherePlatformType {
		return "", fmt.Errorf("unsupported platform: %s", infra.Status.PlatformStatus.Type)
	}

	cloudConfigMap, err := c.controller.cloudConfigMapLister.ConfigMaps(cloudConfigNamespace).Get(infra.Spec.CloudConfig.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get cluster config: %s", err)
	}

	cfgString, found := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !found {
		return "", fmt.Errorf("cluster config %s/%s does not contain key %q", cloudConfigNamespace, infra.Spec.CloudConfig.Name, infra.Spec.CloudConfig.Key)
	}
	klog.V(4).Infof("Got ConfigMap %s/%s with config:\n%s", cloudConfigNamespace, infra.Spec.CloudConfig.Name, cfgString)

	return cfgString, nil
}

func (c *vSphereChecker) enqueueClusterChecks(checkContext *check.CheckContext, checkRunner *CheckThreadPool, resultCollector *ResultCollector) {
	for name, checkFunc := range c.controller.clusterChecks {
		name := name
		checkFunc := checkFunc
		checkRunner.RunGoroutine(checkContext.Context, func() {
			runSingleClusterCheck(checkContext, name, checkFunc, resultCollector)
		})
	}
}

func (c *vSphereChecker) enqueueNodeChecks(checkContext *check.CheckContext, checkRunner *CheckThreadPool, resultCollector *ResultCollector) error {
	nodes, err := c.controller.ListNodes(checkContext.Context)
	if err != nil {
		return err
	}

	for _, nodeCheck := range c.controller.nodeChecks {
		nodeCheck.StartCheck()
	}

	for i := range nodes {
		node := nodes[i]
		c.enqueueSingleNodeChecks(checkContext, checkRunner, resultCollector, node)
	}
	return nil
}

func (c *vSphereChecker) enqueueSingleNodeChecks(checkContext *check.CheckContext, checkRunner *CheckThreadPool, resultCollector *ResultCollector, node *v1.Node) {
	// Run a go routine that reads VM from vSphere and schedules separate goroutines for each check.
	checkRunner.RunGoroutine(checkContext.Context, func() {
		// Try to get VM
		vm, err := getVM(checkContext, node)
		if err != nil {
			// mark all checks as failed
			for _, check := range c.controller.nodeChecks {
				res := checkResult{
					Name:  check.Name(),
					Error: err,
				}
				resultCollector.AddResult(res)
			}
			return
		}
		// We got the VM, enqueue all node checks
		for i := range c.controller.nodeChecks {
			check := c.controller.nodeChecks[i]
			klog.V(4).Infof("Adding node check %s:%s", node.Name, check.Name())
			runSingleNodeSingleCheck(checkContext, resultCollector, node, vm, check)
		}
	})
}

func runSingleClusterCheck(checkContext *check.CheckContext, name string, checkFunc check.ClusterCheck, resultCollector *ResultCollector) {
	res := checkResult{
		Name: name,
	}
	klog.V(4).Infof("%s starting", name)
	err := checkFunc(checkContext)
	if err != nil {
		res.Error = err
		clusterCheckErrrorMetric.WithLabelValues(name).Set(1)
		klog.V(2).Infof("%s failed: %s", name, err)
	} else {
		clusterCheckErrrorMetric.WithLabelValues(name).Set(0)
		klog.V(2).Infof("%s passed", name)
	}
	clusterCheckTotalMetric.WithLabelValues(name).Inc()
	resultCollector.AddResult(res)
}

func runSingleNodeSingleCheck(checkContext *check.CheckContext, resultCollector *ResultCollector, node *v1.Node, vm *mo.VirtualMachine, check check.NodeCheck) {
	name := check.Name()
	res := checkResult{
		Name: name,
	}
	klog.V(4).Infof("%s:%s starting ", name, node.Name)
	err := check.CheckNode(checkContext, node, vm)
	if err != nil {
		res.Error = err
		nodeCheckErrrorMetric.WithLabelValues(name, node.Name).Set(1)
		klog.V(2).Infof("%s:%s failed: %s", name, node.Name, err)
	} else {
		nodeCheckErrrorMetric.WithLabelValues(name, node.Name).Set(0)
		klog.V(2).Infof("%s:%s passed", name, node.Name)
	}
	nodeCheckTotalMetric.WithLabelValues(name, node.Name).Inc()
	resultCollector.AddResult(res)
}

func (c *vSphereChecker) finishNodeChecks(ctx *check.CheckContext) {
	for i := range c.controller.nodeChecks {
		check := c.controller.nodeChecks[i]
		check.FinishCheck(ctx)
	}
}

func getVM(checkContext *check.CheckContext, node *v1.Node) (*mo.VirtualMachine, error) {
	tctx, cancel := context.WithTimeout(checkContext.Context, *check.Timeout)
	defer cancel()
	vmUUID := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(node.Spec.ProviderID, "vsphere://")))

	// Find VM reference in the datastore, by UUID
	s := object.NewSearchIndex(checkContext.VMClient)
	tctx, cancel = context.WithTimeout(checkContext.Context, *check.Timeout)
	defer cancel()

	// datacenter can be nil...
	svm, err := s.FindByUuid(tctx, nil, vmUUID, true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find VM by UUID %s: %s", vmUUID, err)
	}

	// Find VM properties
	vm := object.NewVirtualMachine(checkContext.VMClient, svm.Reference())
	tctx, cancel = context.WithTimeout(checkContext.Context, *check.Timeout)
	defer cancel()
	var o mo.VirtualMachine
	err = vm.Properties(tctx, vm.Reference(), check.NodeProperties, &o)
	if err != nil {
		return nil, fmt.Errorf("failed to load VM %s: %s", node.Name, err)
	}

	return &o, nil
}

func parseConfig(data string) (*vsphere.VSphereConfig, error) {
	var cfg vsphere.VSphereConfig
	err := gcfg.ReadStringInto(&cfg, data)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func newClient(ctx context.Context, cfg *vsphere.VSphereConfig, username, password string) (*govmomi.Client, *rest.Client, error) {
	serverAddress := cfg.Workspace.VCenterIP
	serverURL, err := soap.ParseURL(serverAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse config file: %s", err)
	}

	insecure := cfg.Global.InsecureFlag

	tctx, cancel := context.WithTimeout(ctx, *check.Timeout)
	defer cancel()
	klog.V(4).Infof("Connecting to %s as %s, insecure %t", serverAddress, username, insecure)

	// Set user to nil there for prevent login during client creation.
	// See https://github.com/vmware/govmomi/blob/master/client.go#L91
	serverURL.User = nil
	client, err := govmomi.NewClient(tctx, serverURL, insecure)

	if err != nil {
		return nil, nil, err
	}

	// Set up user agent before login for being able to track vpdo component in vcenter sessions list
	vpdVersion := version.Get()
	client.UserAgent = fmt.Sprintf("vsphere-problem-detector/%s", vpdVersion)

	if err := client.Login(tctx, url.UserPassword(username, password)); err != nil {
		return nil, nil, fmt.Errorf("unable to login to vCenter: %w", err)
	}

	restClient := rest.NewClient(client.Client)
	if err := restClient.Login(tctx, url.UserPassword(username, password)); err != nil {
		client.Logout(context.TODO())
		return nil, nil, fmt.Errorf("unable to login to vCenter REST API: %w", err)
	}

	return client, restClient, nil
}
