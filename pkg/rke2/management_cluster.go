/*
Copyright 2022 SUSE.

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

package rke2

import (
	"context"
	"time"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/collections"

	"github.com/rancher-sandbox/cluster-api-provider-rke2/pkg/secret"
)

const (
	// DefaultWorkloadTimeout is the default timeout for the management cluster.
	DefaultWorkloadTimeout = 30 * time.Second
)

// ManagementCluster defines all behaviors necessary for something to function as a management cluster.
type ManagementCluster interface {
	ctrlclient.Reader

	GetMachinesForCluster(ctx context.Context, cluster ctrlclient.ObjectKey, filters ...collections.Func) (collections.Machines, error)
	GetWorkloadCluster(ctx context.Context, clusterKey ctrlclient.ObjectKey) (WorkloadCluster, error)
}

// Management holds operations on the management cluster.
type Management struct {
	Client              ctrlclient.Client
	SecretCachingClient ctrlclient.Reader
	Tracker             *remote.ClusterCacheTracker
}

// RemoteClusterConnectionError represents a failure to connect to a remote cluster.
type RemoteClusterConnectionError struct {
	Name string
	Err  error
}

func (e *RemoteClusterConnectionError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *RemoteClusterConnectionError) Unwrap() error { return e.Err }

// Get implements ctrlclient.Reader.
func (m *Management) Get(ctx context.Context, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
	return m.Client.Get(ctx, key, obj, opts...)
}

// List implements ctrlclient.Reader.
func (m *Management) List(ctx context.Context, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
	return m.Client.List(ctx, list, opts...)
}

// GetMachinesForCluster returns a list of machines that can be filtered or not.
// If no filter is supplied then all machines associated with the target cluster are returned.
func (m *Management) GetMachinesForCluster(
	ctx context.Context,
	cluster ctrlclient.ObjectKey,
	filters ...collections.Func,
) (collections.Machines, error) {
	logger := log.FromContext(ctx)
	selector := map[string]string{
		clusterv1.ClusterNameLabel: cluster.Name,
	}
	ml := &clusterv1.MachineList{}

	logger.V(5).Info("Getting List of machines for Cluster")

	if err := m.Client.List(ctx, ml, ctrlclient.InNamespace(cluster.Namespace), ctrlclient.MatchingLabels(selector)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}

	logger.V(5).Info("End of listing machines for cluster")

	machines := collections.FromMachineList(ml)

	return machines.Filter(filters...), nil
}

const (
	// RKE2ControlPlaneControllerName defines the controller used when creating clients.
	RKE2ControlPlaneControllerName = "rke2-controlplane-controller"
)

// GetWorkloadCluster builds a cluster object.
// The cluster comes with an etcd client generator to connect to any etcd pod living on a managed machine.
func (m *Management) GetWorkloadCluster(ctx context.Context, clusterKey ctrlclient.ObjectKey) (WorkloadCluster, error) {
	restConfig, err := remote.RESTConfig(ctx, RKE2ControlPlaneControllerName, m.Client, clusterKey)
	if err != nil {
		return nil, err
	}

	restConfig.Timeout = DefaultWorkloadTimeout

	c, err := ctrlclient.New(restConfig, ctrlclient.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, &RemoteClusterConnectionError{Name: clusterKey.String(), Err: err}
	}

	return m.NewWorkload(ctx, c, restConfig, clusterKey)
}

func (m *Management) getEtcdCAKeyPair(ctx context.Context, clusterKey ctrlclient.ObjectKey) (*certs.KeyPair, error) {
	etcd := &secret.ManagedCertificate{
		Purpose: secret.EtcdCA,
	}

	// Try to get the certificate via the cached ctrlclient.
	s, err := etcd.Lookup(ctx, m.SecretCachingClient, clusterKey)
	if err != nil || s == nil {
		// Return error if we got an errors which is not a NotFound error.
		return nil, errors.Wrapf(err, "failed to get secret; etcd CA bundle %s/%s", clusterKey.Namespace, secret.Name(clusterKey.Name, secret.EtcdCA))
	}

	return etcd.KeyPair, nil
}

func (m *Management) getRemoteKeyPair(ctx context.Context, remoteClient ctrlclient.Client, clusterKey ctrlclient.ObjectKey) (*certs.KeyPair, error) {
	etcdCertificate := &secret.ExternalCertificate{
		Reader:  remoteClient,
		Purpose: secret.EtcdCA,
	}
	externalCertificates := secret.Certificates{etcdCertificate}

	if err := externalCertificates.LookupOrGenerate(ctx, m.Client, clusterKey, metav1.OwnerReference{}); err != nil {
		log.FromContext(ctx).Error(err, "unable to lookup or create cluster certificates")

		return nil, err
	}

	return etcdCertificate.KeyPair, nil
}
