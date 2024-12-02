/*
Copyright 2024.

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

package loopback

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/openshift-kni/oran-hwmgr-plugin/internal/controller/utils"
	hwmgmtv1alpha1 "github.com/openshift-kni/oran-o2ims/api/hardwaremanagement/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// AllocateNode processes a NodePool CR, allocating a free node for each specified nodegroup as needed
func (a *Adaptor) AllocateNode(ctx context.Context, nodepool *hwmgmtv1alpha1.NodePool) error {
	cloudID := nodepool.Spec.CloudID

	// Inject a delay before allocating node
	time.Sleep(10 * time.Second)

	cm, resources, allocations, err := a.GetCurrentResources(ctx)
	if err != nil {
		return fmt.Errorf("unable to get current resources: %w", err)
	}

	var cloud *cmAllocatedCloud
	for i, iter := range allocations.Clouds {
		if iter.CloudID == cloudID {
			cloud = &allocations.Clouds[i]
			break
		}
	}
	if cloud == nil {
		// The cloud wasn't found in the list, so create a new entry
		allocations.Clouds = append(allocations.Clouds, cmAllocatedCloud{CloudID: cloudID, Nodegroups: make(map[string][]string)})
		cloud = &allocations.Clouds[len(allocations.Clouds)-1]
	}

	// Check available resources
	for _, nodegroup := range nodepool.Spec.NodeGroup {
		used := cloud.Nodegroups[nodegroup.NodePoolData.Name]
		remaining := nodegroup.Size - len(used)
		if remaining <= 0 {
			// This group is allocated
			a.Logger.InfoContext(ctx, "nodegroup is fully allocated", "nodegroup", nodegroup.NodePoolData.Name)
			continue
		}

		freenodes := getFreeNodesInPool(resources, allocations, nodegroup.NodePoolData.ResourcePoolId)
		if remaining > len(freenodes) {
			return fmt.Errorf("not enough free resources remaining in resource pool %s", nodegroup.NodePoolData.ResourcePoolId)
		}

		// Grab the first node
		nodename := freenodes[0]

		nodeinfo, exists := resources.Nodes[nodename]
		if !exists {
			return fmt.Errorf("unable to find nodeinfo for %s", nodename)
		}

		if err := a.CreateBMCSecret(ctx, nodename, nodeinfo.BMC.UsernameBase64, nodeinfo.BMC.PasswordBase64); err != nil {
			return fmt.Errorf("failed to create bmc-secret when allocating node %s: %w", nodename, err)
		}

		cloud.Nodegroups[nodegroup.NodePoolData.Name] = append(cloud.Nodegroups[nodegroup.NodePoolData.Name], nodename)

		// Update the configmap
		yamlString, err := yaml.Marshal(&allocations)
		if err != nil {
			return fmt.Errorf("unable to marshal allocated data: %w", err)
		}
		cm.Data[allocationsKey] = string(yamlString)
		if err := a.Client.Update(ctx, cm); err != nil {
			return fmt.Errorf("failed to update configmap: %w", err)
		}

		if err := a.CreateNode(ctx, cloudID, nodename, nodegroup.NodePoolData.Name, nodegroup.NodePoolData.HwProfile); err != nil {
			return fmt.Errorf("failed to create allocated node (%s): %w", nodename, err)
		}

		if err := a.UpdateNodeStatus(ctx, nodename, nodeinfo, nodegroup.NodePoolData.HwProfile); err != nil {
			return fmt.Errorf("failed to update node status (%s): %w", nodename, err)
		}
	}

	return nil
}

func bmcSecretName(nodename string) string {
	return fmt.Sprintf("%s-bmc-secret", nodename)
}

// CreateBMCSecret creates the bmc-secret for a node
func (a *Adaptor) CreateBMCSecret(ctx context.Context, nodename, usernameBase64, passwordBase64 string) error {
	a.Logger.InfoContext(ctx, "Creating bmc-secret:", "nodename", nodename)

	secretName := bmcSecretName(nodename)

	username, err := base64.StdEncoding.DecodeString(usernameBase64)
	if err != nil {
		return fmt.Errorf("failed to decode usernameBase64 string (%s) for node %s: %w", usernameBase64, nodename, err)
	}

	password, err := base64.StdEncoding.DecodeString(passwordBase64)
	if err != nil {
		return fmt.Errorf("failed to decode usernameBase64 string (%s) for node %s: %w", passwordBase64, nodename, err)
	}

	bmcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: a.Namespace,
		},
		Data: map[string][]byte{
			"username": username,
			"password": password,
		},
	}

	if err = utils.CreateK8sCR(ctx, a.Client, bmcSecret, nil, utils.UPDATE); err != nil {
		return fmt.Errorf("failed to create bmc-secret for node %s: %w", nodename, err)
	}

	return nil
}

// DeleteBMCSecret deletes the bmc-secret for a node
func (a *Adaptor) DeleteBMCSecret(ctx context.Context, nodename string) error {
	a.Logger.InfoContext(ctx, "Deleting bmc-secret:", "nodename", nodename)

	secretName := bmcSecretName(nodename)

	bmcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: a.Namespace,
		},
	}

	if err := a.Client.Delete(ctx, bmcSecret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete bmc-secret for node %s: %w", nodename, err)
	}

	return nil
}

// CreateNode creates a Node CR with specified attributes
func (a *Adaptor) CreateNode(ctx context.Context, cloudID, nodename, groupname, hwprofile string) error {

	a.Logger.InfoContext(ctx, "Creating node:",
		"cloudID", cloudID,
		"nodegroup name", groupname,
		"nodename", nodename,
	)

	node := &hwmgmtv1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodename,
			Namespace: a.Namespace,
		},
		Spec: hwmgmtv1alpha1.NodeSpec{
			NodePool:  cloudID,
			GroupName: groupname,
			HwProfile: hwprofile,
		},
	}

	if err := a.Client.Create(ctx, node); err != nil {
		return fmt.Errorf("failed to create Node: %w", err)
	}

	return nil
}

// UpdateNodeStatus updates a Node CR status field with additional node information from the nodelist configmap
func (a *Adaptor) UpdateNodeStatus(ctx context.Context, nodename string, info cmNodeInfo, hwprofile string) error {

	a.Logger.InfoContext(ctx, "Updating node:",
		"nodename", nodename,
	)

	node := &hwmgmtv1alpha1.Node{}

	if err := utils.RetryOnConflictOrRetriableOrNotFound(retry.DefaultRetry, func() error {
		return a.Get(ctx, types.NamespacedName{Name: nodename, Namespace: a.Namespace}, node)
	}); err != nil {
		return fmt.Errorf("failed to get Node for update: %w", err)
	}

	a.Logger.InfoContext(ctx, "Adding info to node", "nodename", nodename, "info", info)
	node.Status.BMC = &hwmgmtv1alpha1.BMC{
		Address:         info.BMC.Address,
		CredentialsName: bmcSecretName(nodename),
	}
	node.Status.Interfaces = info.Interfaces

	utils.SetStatusCondition(&node.Status.Conditions,
		string(hwmgmtv1alpha1.Provisioned),
		string(hwmgmtv1alpha1.Completed),
		metav1.ConditionTrue,
		"Provisioned")
	node.Status.HwProfile = hwprofile
	if err := utils.UpdateK8sCRStatus(ctx, a.Client, node); err != nil {
		return fmt.Errorf("failed to update status for node %s: %w", nodename, err)
	}

	return nil
}

// DeleteNode deletes a Node CR
func (a *Adaptor) DeleteNode(ctx context.Context, nodename string) error {

	a.Logger.InfoContext(ctx, "Deleting node:",
		"nodename", nodename,
	)

	node := &hwmgmtv1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodename,
			Namespace: a.Namespace,
		},
	}

	if err := a.Client.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete Node: %w", err)
	}

	return nil
}

// GetNode get a node resource for a provided name
func (a *Adaptor) GetNode(ctx context.Context, nodename string) (*hwmgmtv1alpha1.Node, error) {

	a.Logger.InfoContext(ctx, "Getting node:",
		"nodename", nodename,
	)

	node := &hwmgmtv1alpha1.Node{}

	if err := utils.RetryOnConflictOrRetriableOrNotFound(retry.DefaultRetry, func() error {
		return a.Get(ctx, types.NamespacedName{Name: nodename, Namespace: a.Namespace}, node)
	}); err != nil {
		return node, fmt.Errorf("failed to get Node for update: %w", err)
	}
	return node, nil
}
