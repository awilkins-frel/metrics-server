// Copyright 2018 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package app

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/spf13/cobra"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubernetes-incubator/metrics-server/pkg/apiserver"
	"github.com/kubernetes-incubator/metrics-server/pkg/manager"
	"github.com/kubernetes-incubator/metrics-server/pkg/provider"
	"github.com/kubernetes-incubator/metrics-server/pkg/sources"
	"github.com/kubernetes-incubator/metrics-server/pkg/sources/summary"
)

// NewCommandStartMetricsServer provides a CLI handler for the metrics server entrypoint
func NewCommandStartMetricsServer(out, errOut io.Writer, stopCh <-chan struct{}) *cobra.Command {
	o := NewMetricsServerOptions()

	cmd := &cobra.Command{
		Short: "Launch metrics-server",
		Long:  "Launch metrics-server",
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Run(stopCh); err != nil {
				return err
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.DurationVar(&o.MetricResolution, "metric-resolution", o.MetricResolution, "The resolution at which metrics-server will retain metrics.")

	flags.BoolVar(&o.InsecureKubelet, "kubelet-insecure", o.InsecureKubelet, "Do not connect to Kubelet using HTTPS")
	flags.IntVar(&o.KubeletPort, "kubelet-port", o.KubeletPort, "The port to use to connect to Kubelets (defaults to 10250)")
	flags.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "The path to the kubeconfig used to connect to the Kubernetes API server and the Kubelets (defaults to in-cluster config)")

	o.SecureServing.AddFlags(flags)
	o.Authentication.AddFlags(flags)
	o.Authorization.AddFlags(flags)
	o.Features.AddFlags(flags)

	return cmd
}

type MetricsServerOptions struct {
	// genericoptions.ReccomendedOptions - EtcdOptions
	SecureServing  *genericoptions.SecureServingOptions
	Authentication *genericoptions.DelegatingAuthenticationOptions
	Authorization  *genericoptions.DelegatingAuthorizationOptions
	Features       *genericoptions.FeatureOptions

	Kubeconfig string

	// Only to be used to for testing
	DisableAuthForTesting bool

	MetricResolution time.Duration
	KubeletPort      int
	InsecureKubelet  bool
}

// NewMetricsServerOptions constructs a new set of default options for metrics-server.
func NewMetricsServerOptions() *MetricsServerOptions {
	o := &MetricsServerOptions{
		SecureServing:  genericoptions.NewSecureServingOptions(),
		Authentication: genericoptions.NewDelegatingAuthenticationOptions(),
		Authorization:  genericoptions.NewDelegatingAuthorizationOptions(),
		Features:       genericoptions.NewFeatureOptions(),

		MetricResolution: 60 * time.Second,
		KubeletPort:      10250,
	}

	return o
}

func (o MetricsServerOptions) Config() (*apiserver.Config, error) {
	if err := o.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	serverConfig := genericapiserver.NewConfig(apiserver.Codecs)
	if err := o.SecureServing.ApplyTo(&serverConfig.SecureServing); err != nil {
		return nil, err
	}

	if !o.DisableAuthForTesting {
		if err := o.Authentication.ApplyTo(&serverConfig.Authentication, serverConfig.SecureServing, nil); err != nil {
			return nil, err
		}
		if err := o.Authorization.ApplyTo(&serverConfig.Authorization); err != nil {
			return nil, err
		}
	}

	serverConfig.SwaggerConfig = genericapiserver.DefaultSwaggerConfig()
	return &apiserver.Config{
		GenericConfig:  serverConfig,
		ProviderConfig: apiserver.ProviderConfig{},
	}, nil
}

func (o MetricsServerOptions) Run(stopCh <-chan struct{}) error {
	// grab the config for the API server
	config, err := o.Config()
	if err != nil {
		return err
	}
	config.GenericConfig.EnableMetrics = true

	// set up the client config
	var clientConfig *rest.Config
	if len(o.Kubeconfig) > 0 {
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: o.Kubeconfig}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

		clientConfig, err = loader.ClientConfig()
	} else {
		clientConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("unable to construct lister client config: %v", err)
	}

	// set up the informers
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("unable to construct lister client: %v", err)
	}
	// we should never need to resync, since we're not worried about missing events,
	// and resync is actually for regular interval-based reconciliation these days,
	// so set the default resync interval to 0
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)

	// set up the source manager
	kubeletConfig := summary.GetKubeletConfig(clientConfig, o.KubeletPort)
	kubeletClient, err := summary.KubeletClientFor(kubeletConfig)
	if err != nil {
		return fmt.Errorf("unable to construct a client to connect to the kubelets: %v", err)
	}
	sourceProvider := summary.NewSummaryProvider(informerFactory.Core().V1().Nodes().Lister(), kubeletClient)
	sourceManager, err := sources.NewSourceManager(sourceProvider, sources.DefaultMetricsScrapeTimeout)
	if err != nil {
		return fmt.Errorf("unable to initialize source manager: %v", err)
	}

	// set up the in-memory sink and provider
	metricSink, metricsProvider := provider.NewSinkProvider()

	// set up the general manager
	mgr := manager.NewManager(sourceManager, metricSink, o.MetricResolution)
	if err != nil {
		return fmt.Errorf("unable to create main manager: %v", err)
	}

	// inject the providers into the config
	config.ProviderConfig.Node = metricsProvider
	config.ProviderConfig.Pod = metricsProvider

	// complete the config to get an API server
	server, err := config.Complete(informerFactory).New()
	if err != nil {
		return err
	}

	// add health checks
	server.AddHealthzChecks(healthz.NamedCheck("healthz", mgr.CheckHealth))

	// run everything (the apiserver runs the shared informer factory for us)
	mgr.RunUntil(stopCh)
	return server.GenericAPIServer.PrepareRun().Run(stopCh)
}
