/*
Copyright 2022 The Kubernetes Authors.

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

// Package cmd defines a root command for the kwok.
package cmd

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"

	"sigs.k8s.io/kwok/pkg/apis/internalversion"
	"sigs.k8s.io/kwok/pkg/cni"
	"sigs.k8s.io/kwok/pkg/config"
	"sigs.k8s.io/kwok/pkg/consts"
	"sigs.k8s.io/kwok/pkg/kwok/controllers"
	"sigs.k8s.io/kwok/pkg/kwok/controllers/templates"
	"sigs.k8s.io/kwok/pkg/log"
	"sigs.k8s.io/kwok/pkg/utils/envs"
	"sigs.k8s.io/kwok/pkg/utils/path"
)

type flagpole struct {
	Kubeconfig string
	Master     string

	*internalversion.KwokConfiguration
}

// NewCommand returns a new cobra.Command for root
func NewCommand(ctx context.Context) *cobra.Command {
	flags := &flagpole{}
	flags.KwokConfiguration = config.GetKwokConfiguration(ctx)

	cmd := &cobra.Command{
		Args:          cobra.NoArgs,
		Use:           "kwok [command]",
		Short:         "kwok is a tool for simulate thousands of fake kubelets",
		Long:          "kwok is a tool for simulate thousands of fake kubelets",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       consts.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			logger := log.FromContext(ctx)

			if flags.Kubeconfig != "" {
				flags.Kubeconfig = path.ExpandHome(flags.Kubeconfig)
				f, err := os.Stat(flags.Kubeconfig)
				if err != nil || f.IsDir() {
					logger.Warn("Failed to get kubeconfig file or it is a directory", "kubeconfig", flags.Kubeconfig)
					flags.Kubeconfig = ""
				}
			}

			clientset, err := newClientset(ctx, flags.Master, flags.Kubeconfig)
			if err != nil {
				return err
			}

			if flags.Options.ManageAllNodes {
				if flags.Options.ManageNodesWithAnnotationSelector != "" || flags.Options.ManageNodesWithLabelSelector != "" {
					logger.Error("manage-all-nodes is conflicted with manage-nodes-with-annotation-selector and manage-nodes-with-label-selector.", nil)
					os.Exit(1)
				}
				logger.Info("Watch all nodes")
			} else if flags.Options.ManageNodesWithAnnotationSelector != "" || flags.Options.ManageNodesWithLabelSelector != "" {
				logger.Info("Watch nodes",
					"annotation", flags.Options.ManageNodesWithAnnotationSelector,
					"label", flags.Options.ManageNodesWithLabelSelector,
				)
			}

			backoff := wait.Backoff{
				Duration: 1 * time.Second,
				Factor:   2,
				Jitter:   0.1,
				Steps:    5,
			}
			err = wait.ExponentialBackoffWithContext(ctx, backoff,
				func() (bool, error) {
					_, err := clientset.CoreV1().Nodes().List(ctx,
						metav1.ListOptions{
							Limit: 1,
						})
					if err != nil {
						logger.Error("Failed to list nodes", err)
						return false, nil
					}
					return true, nil
				},
			)
			if err != nil {
				return err
			}

			ctr, err := controllers.NewController(controllers.Config{
				ClientSet:                             clientset,
				EnableCNI:                             flags.Options.EnableCNI,
				ManageAllNodes:                        flags.Options.ManageAllNodes,
				ManageNodesWithAnnotationSelector:     flags.Options.ManageNodesWithAnnotationSelector,
				ManageNodesWithLabelSelector:          flags.Options.ManageNodesWithLabelSelector,
				DisregardStatusWithAnnotationSelector: flags.Options.DisregardStatusWithAnnotationSelector,
				DisregardStatusWithLabelSelector:      flags.Options.DisregardStatusWithLabelSelector,
				CIDR:                                  flags.Options.CIDR,
				NodeIP:                                flags.Options.NodeIP,
				PodStatusTemplate:                     templates.DefaultPodStatusTemplate,
				NodeHeartbeatTemplate:                 templates.DefaultNodeHeartbeatTemplate,
				NodeInitializationTemplate:            templates.DefaultNodeStatusTemplate,
			})
			if err != nil {
				return err
			}

			if flags.Options.ServerAddress != "" {
				go Serve(ctx, flags.Options.ServerAddress)
			}

			err = ctr.Start(ctx)
			if err != nil {
				return err
			}

			<-ctx.Done()
			return nil
		},
	}

	flags.Kubeconfig = envs.GetEnv("KUBECONFIG", flags.Kubeconfig)

	cmd.Flags().StringVar(&flags.Options.CIDR, "cidr", flags.Options.CIDR, "CIDR of the pod ip")
	cmd.Flags().StringVar(&flags.Options.NodeIP, "node-ip", flags.Options.NodeIP, "IP of the node")
	cmd.Flags().BoolVar(&flags.Options.ManageAllNodes, "manage-all-nodes", flags.Options.ManageAllNodes, "All nodes will be watched and managed. It's conflicted with manage-nodes-with-annotation-selector and manage-nodes-with-label-selector.")
	cmd.Flags().StringVar(&flags.Options.ManageNodesWithAnnotationSelector, "manage-nodes-with-annotation-selector", flags.Options.ManageNodesWithAnnotationSelector, "Nodes that match the annotation selector will be watched and managed. It's conflicted with manage-all-nodes.")
	cmd.Flags().StringVar(&flags.Options.ManageNodesWithLabelSelector, "manage-nodes-with-label-selector", flags.Options.ManageNodesWithLabelSelector, "Nodes that match the label selector will be watched and managed. It's conflicted with manage-all-nodes.")
	cmd.Flags().StringVar(&flags.Options.DisregardStatusWithAnnotationSelector, "disregard-status-with-annotation-selector", flags.Options.DisregardStatusWithAnnotationSelector, "All node/pod status excluding the ones that match the annotation selector will be watched and managed.")
	cmd.Flags().StringVar(&flags.Options.DisregardStatusWithLabelSelector, "disregard-status-with-label-selector", flags.Options.DisregardStatusWithLabelSelector, "All node/pod status excluding the ones that match the label selector will be watched and managed.")
	cmd.Flags().StringVar(&flags.Kubeconfig, "kubeconfig", flags.Kubeconfig, "Path to the kubeconfig file to use")
	cmd.Flags().StringVar(&flags.Master, "master", flags.Master, "Server is the address of the kubernetes cluster")
	cmd.Flags().StringVar(&flags.Options.ServerAddress, "server-address", flags.Options.ServerAddress, "Address to expose health and metrics on")

	if cni.SupportedCNI() {
		cmd.Flags().BoolVar(&flags.Options.EnableCNI, "experimental-enable-cni", flags.Options.EnableCNI, "Experimental support for getting pod ip from CNI, for CNI-related components")
	}
	return cmd
}

func Serve(ctx context.Context, address string) {
	logger := log.FromContext(ctx)
	promHandler := promhttp.Handler()
	svc := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
		Addr: address,
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz", "/readyz", "/livez":
				_, err := rw.Write([]byte("ok"))
				if err != nil {
					logger.Error("Failed to write", err)
				}
			case "/metrics":
				promHandler.ServeHTTP(rw, r)
			default:
				http.NotFound(rw, r)
			}
		}),
	}

	err := svc.ListenAndServe()
	if err != nil {
		logger.Error("Fatal start server", err)
		os.Exit(1)
	}
}

// buildConfigFromFlags is a helper function that builds configs from a master url or a kubeconfig filepath.
func buildConfigFromFlags(ctx context.Context, masterURL, kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" && masterURL == "" {
		logger := log.FromContext(ctx)
		logger.Warn("Neither --kubeconfig nor --master was specified")
		logger.Info("Using the inClusterConfig")
		kubeconfig, err := rest.InClusterConfig()
		if err == nil {
			return kubeconfig, nil
		}
		logger.Error("Creating inClusterConfig", err)
		logger.Info("Falling back to default config")
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}}).ClientConfig()
}

func newClientset(ctx context.Context, master, kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := buildConfigFromFlags(ctx, master, kubeconfig)
	if err != nil {
		return nil, err
	}
	err = setConfigDefaults(cfg)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func setConfigDefaults(config *rest.Config) error {
	config.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	return rest.SetKubernetesDefaults(config)
}
