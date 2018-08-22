package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/google/uuid"
)

func newRandomClusterID() string {
	uuid, _ := uuid.NewRandom()
	return uuid.String()[0:8]
}

type ClusterOptions struct {
	Timeout time.Duration
	Nodes   []NodeOptions
}

type Node struct {
	ContainerID          string
	State                string
	Name                 string
	InitialServerVersion string
	IPv4Address          string
	IPv6Address          string
}

type Cluster struct {
	ID      string
	Creator string
	Owner   string
	Timeout time.Time
	Nodes   []*Node
}

func getCluster(ctx context.Context, clusterID string) (*Cluster, error) {
	clusters, err := getAllClusters(ctx)
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		if cluster.ID == clusterID {
			return cluster, nil
		}
	}

	return nil, errors.New("cluster not found")
}

func getAllClusters(ctx context.Context) ([]*Cluster, error) {
	containers, err := docker.ContainerList(ctx, types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	clusterMap := make(map[string][]types.Container)

	for _, container := range containers {
		clusterID := container.Labels["com.couchbase.dyncluster.cluster_id"]
		if clusterID != "" {
			clusterMap[clusterID] = append(clusterMap[clusterID], container)
		}
	}

	var clusters []*Cluster
	for clusterID, containers := range clusterMap {
		meta, err := metaStore.GetClusterMeta(clusterID)
		if err != nil {
			log.Printf("Encountered unregistered cluster: %s", clusterID)
		}

		clusterCreator := ""

		var nodes []*Node
		for _, container := range containers {
			eth0Net := container.NetworkSettings.Networks["macvlan0"]
			if eth0Net == nil {
				// This is a little hack to make sure wierd stuff doesn't stop the node
				// from showing up in the nodes list
				fakeNet := network.EndpointSettings{}
				eth0Net = &fakeNet
			}

			containerCreator := container.Labels["com.couchbase.dyncluster.creator"]
			if clusterCreator == "" {
				clusterCreator = containerCreator
			}

			nodes = append(nodes, &Node{
				ContainerID:          container.ID[0:12],
				State:                container.State,
				Name:                 container.Labels["com.couchbase.dyncluster.node_name"],
				InitialServerVersion: container.Labels["com.couchbase.dyncluster.initial_server_version"],
				IPv4Address:          eth0Net.IPAddress,
				IPv6Address:          eth0Net.GlobalIPv6Address,
			})
		}

		if clusterCreator == "" {
			clusterCreator = "unknown"
		}

		// Don't include clusters that we don't actually own
		if !ContextIgnoreOwnership(ctx) && clusterCreator != ContextUser(ctx) {
			continue
		}

		clusters = append(clusters, &Cluster{
			ID:      clusterID,
			Creator: clusterCreator,
			Owner:   meta.Owner,
			Timeout: meta.Timeout,
			Nodes:   nodes,
		})
	}

	return clusters, nil
}

func allocateCluster(ctx context.Context, opts ClusterOptions) (string, error) {
	log.Printf("Allocating cluster (requested by: %s)", ContextUser(ctx))

	if opts.Timeout < 0 {
		return "", errors.New("must specify a valid timeout for the cluster")
	}
	if opts.Timeout > 2*7*24*time.Hour {
		return "", errors.New("cannot allocate clusters for longer than 2 weeks")
	}
	if len(opts.Nodes) == 0 {
		return "", errors.New("must specify at least a single node for the cluster")
	}
	if len(opts.Nodes) > 10 {
		return "", errors.New("cannot allocate clusters with more than 10 nodes")
	}

	clusterID := newRandomClusterID()
	timeoutTime := time.Now().Add(1 * time.Hour)

	meta := ClusterMeta{
		Owner:   ContextUser(ctx),
		Timeout: timeoutTime,
	}
	err := metaStore.CreateClusterMeta(clusterID, meta)
	if err != nil {
		return "", err
	}

	var nodesToAllocate []NodeOptions
	for nodeIdx, node := range opts.Nodes {
		if node.Name == "" {
			node.Name = fmt.Sprintf("node_%d", nodeIdx+1)
		}

		nodesToAllocate = append(nodesToAllocate, node)
	}

	signal := make(chan error)

	for _, node := range nodesToAllocate {
		go func(clusterID string, node NodeOptions) {
			_, err := allocateNode(ctx, clusterID, timeoutTime, node)
			signal <- err
		}(clusterID, node)
	}

	var createError error
	for range nodesToAllocate {
		err := <-signal
		if err != nil && createError == nil {
			createError = err
		}
	}
	if createError != nil {
		killCluster(ctx, clusterID)
		return "", createError
	}

	return clusterID, nil
}

func refreshCluster(ctx context.Context, clusterID string, newTimeout time.Duration) error {
	log.Printf("Refreshing cluster %s (requested by: %s)", clusterID, ContextUser(ctx))

	// Check the cluster actuall exists
	_, err := getCluster(ctx, clusterID)
	if err != nil {
		return err
	}

	newMeta := ClusterMeta{
		Owner:   ContextUser(ctx),
		Timeout: time.Now().Add(newTimeout),
	}

	_, err = metaStore.GetClusterMeta(clusterID)
	if err != nil {
		// If we failed to fetch the cluster metadata, just insert some instead
		return metaStore.CreateClusterMeta(clusterID, newMeta)
	}

	return metaStore.UpdateClusterMeta(clusterID, func(meta ClusterMeta) (ClusterMeta, error) {
		meta.Owner = newMeta.Owner
		if meta.Timeout.Before(newMeta.Timeout) {
			meta.Timeout = newMeta.Timeout
		}
		return meta, nil
	})
}

func killCluster(ctx context.Context, clusterID string) error {
	log.Printf("Killing cluster %s (requested by: %s)", clusterID, ContextUser(ctx))

	cluster, err := getCluster(ctx, clusterID)
	if err != nil {
		return err
	}

	if !ContextIgnoreOwnership(ctx) && cluster.Owner != ContextUser(ctx) {
		return errors.New("cannot kill clusters you don't own")
	}

	var nodesToKill []string
	for _, node := range cluster.Nodes {
		nodesToKill = append(nodesToKill, node.ContainerID)
	}

	signal := make(chan error)

	for _, nodeID := range nodesToKill {
		go func(nodeID string) {
			signal <- killNode(ctx, nodeID)
		}(nodeID)
	}

	var killError error
	for range nodesToKill {
		err := <-signal
		if err != nil && killError == nil {
			killError = err
		}
	}
	if killError != nil {
		return killError
	}

	return nil
}

func killAllClusters(ctx context.Context) error {
	log.Printf("Killing all clusters")

	var clustersToKill []string

	clusters, err := getAllClusters(ctx)
	if err != nil {
		return err
	}

	for _, cluster := range clusters {
		clustersToKill = append(clustersToKill, cluster.ID)
	}

	signal := make(chan error)

	for _, clusterID := range clustersToKill {
		go func(clusterID string) {
			signal <- killCluster(ctx, clusterID)
		}(clusterID)
	}

	var killError error
	for range clustersToKill {
		err := <-signal
		if err != nil && killError == nil {
			killError = err
		}
	}
	if killError != nil {
		return killError
	}

	return nil
}