package azure

import (
	"context"
	"fmt"
	"log"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-03-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-08-01/network"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
)

type AzureClient struct {
	environment    azure.Environment
	subscriptionID string
	groupsClient   resources.GroupsClient
	vmClient       compute.VirtualMachinesClient
	nicClient      network.InterfacesClient
	subnetsClient  network.SubnetsClient
	vnetClient     network.VirtualNetworksClient
}

func GetAzureClient(cloud, subscriptionID, clientID, tenantID, clientSecret string) (*AzureClient, error) {
	env, err := azure.EnvironmentFromName(cloud)
	if err != nil {
		return nil, err
	}

	oauthConfig, err := getOAuthConfig(env, subscriptionID, tenantID)
	if err != nil {
		return nil, err
	}

	armSpt, err := adal.NewServicePrincipalToken(*oauthConfig, clientID, clientSecret, env.ServiceManagementEndpoint)
	if err != nil {
		return nil, err
	}

	return getClient(env, subscriptionID, tenantID, armSpt), nil
}

func (az *AzureClient) EnsureResourceGroup(ctx context.Context, name, location string, managedBy *string) (resourceGroup *resources.Group, err error) {
	var tags map[string]*string
	group, err := az.groupsClient.Get(ctx, name)
	if err == nil {
		tags = group.Tags
	}

	response, err := az.groupsClient.CreateOrUpdate(ctx, name, resources.Group{
		Name:      &name,
		Location:  &location,
		ManagedBy: managedBy,
		Tags:      tags,
	})
	if err != nil {
		return &response, err
	}

	return &response, nil
}

func (az *AzureClient) DeleteResourceGroup(ctx context.Context, groupName string) error {
	_, err := az.groupsClient.Get(ctx, groupName)
	if err == nil {
		future, err := az.groupsClient.Delete(ctx, groupName)
		if err != nil {
			return fmt.Errorf("cannot delete resource group %v: %v", groupName, err)
		}
		err = future.WaitForCompletionRef(ctx, az.groupsClient.Client)
		if err != nil {
			// Skip the teardown errors because of https://github.com/Azure/go-autorest/issues/357
			// TODO(feiskyer): fix the issue by upgrading go-autorest version >= v11.3.2.
			log.Printf("Warning: failed to delete resource group %q with error %v", groupName, err)
		}
	}
	return nil
}

func (az *AzureClient) EnsureVirtualMachine(ctx context.Context, groupName, location, vmName string) (vm compute.VirtualMachine, err error) {
	nic, err := az.EnsureNIC(ctx, groupName, location, vmName+"-nic", vmName+"-vnet", vmName+"-subnet")
	if err != nil {
		return vm, err
	}

	future, err := az.vmClient.CreateOrUpdate(
		ctx,
		groupName,
		vmName,
		compute.VirtualMachine{
			Location: to.StringPtr(location),
			VirtualMachineProperties: &compute.VirtualMachineProperties{
				HardwareProfile: &compute.HardwareProfile{
					VMSize: compute.VirtualMachineSizeTypesStandardF2,
				},
				StorageProfile: &compute.StorageProfile{
					ImageReference: &compute.ImageReference{
						Publisher: to.StringPtr("Canonical"),
						Offer:     to.StringPtr("UbuntuServer"),
						Sku:       to.StringPtr("16.04.0-LTS"),
						Version:   to.StringPtr("latest"),
					},
				},
				OsProfile: &compute.OSProfile{
					ComputerName:  to.StringPtr(vmName),
					AdminUsername: to.StringPtr("azureuser"),
					AdminPassword: to.StringPtr("Azureuser1234"),
				},
				NetworkProfile: &compute.NetworkProfile{
					NetworkInterfaces: &[]compute.NetworkInterfaceReference{
						{
							ID: nic.ID,
							NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
								Primary: to.BoolPtr(true),
							},
						},
					},
				},
			},
		},
	)
	if err != nil {
		return vm, fmt.Errorf("cannot create vm: %v", err)
	}

	err = future.WaitForCompletionRef(ctx, az.vmClient.Client)
	if err != nil {
		return vm, fmt.Errorf("cannot get the vm create or update future response: %v", err)
	}

	return future.Result(az.vmClient)
}

func (az *AzureClient) EnsureNIC(ctx context.Context, groupName, location, nicName, vnetName, subnetName string) (nic network.Interface, err error) {
	_, err = az.EnsureVirtualNetworkAndSubnet(ctx, groupName, location, vnetName, subnetName)
	if err != nil {
		return nic, err
	}

	subnet, err := az.GetVirtualNetworkSubnet(ctx, groupName, vnetName, subnetName)
	if err != nil {
		return nic, fmt.Errorf("cannot get subnet %s of virtual network %s in %s: %v", subnetName, vnetName, groupName, err)
	}

	future, err := az.nicClient.CreateOrUpdate(
		ctx,
		groupName,
		nicName,
		network.Interface{
			Name:     to.StringPtr(nicName),
			Location: to.StringPtr(location),
			InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
				IPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						Name: to.StringPtr("ipConfig1"),
						InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
							Subnet:                    &subnet,
							PrivateIPAllocationMethod: network.Dynamic,
						},
					},
				},
			},
		},
	)
	if err != nil {
		return nic, fmt.Errorf("cannot create nic: %v", err)
	}

	err = future.WaitForCompletionRef(ctx, az.nicClient.Client)
	if err != nil {
		return nic, fmt.Errorf("cannot get nic create or update future response: %v", err)
	}

	return future.Result(az.nicClient)
}

func (az *AzureClient) EnsureVirtualNetworkAndSubnet(ctx context.Context, groupName, location, vnetName, subnetName string) (vnet network.VirtualNetwork, err error) {
	future, err := az.vnetClient.CreateOrUpdate(
		ctx,
		groupName,
		vnetName,
		network.VirtualNetwork{
			Location: to.StringPtr(location),
			VirtualNetworkPropertiesFormat: &network.VirtualNetworkPropertiesFormat{
				AddressSpace: &network.AddressSpace{
					AddressPrefixes: &[]string{"10.0.0.0/8"},
				},
				Subnets: &[]network.Subnet{
					{
						Name: to.StringPtr(subnetName),
						SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
							AddressPrefix: to.StringPtr("10.0.0.0/16"),
						},
					},
				},
			},
		})

	if err != nil {
		return vnet, fmt.Errorf("cannot create virtual network: %v", err)
	}

	err = future.WaitForCompletionRef(ctx, az.vnetClient.Client)
	if err != nil {
		return vnet, fmt.Errorf("cannot get the vnet create or update future response: %v", err)
	}

	return future.Result(az.vnetClient)
}

func (az *AzureClient) GetVirtualNetworkSubnet(ctx context.Context, groupName, vnetName, subnetName string) (network.Subnet, error) {
	return az.subnetsClient.Get(ctx, groupName, vnetName, subnetName, "")
}

func getOAuthConfig(env azure.Environment, subscriptionID, tenantID string) (*adal.OAuthConfig, error) {
	oauthConfig, err := adal.NewOAuthConfig(env.ActiveDirectoryEndpoint, tenantID)
	if err != nil {
		return nil, err
	}

	return oauthConfig, nil
}

func getClient(env azure.Environment, subscriptionID, tenantID string, armSpt *adal.ServicePrincipalToken) *AzureClient {
	c := &AzureClient{
		environment:    env,
		subscriptionID: subscriptionID,
		groupsClient:   resources.NewGroupsClientWithBaseURI(env.ResourceManagerEndpoint, subscriptionID),
		vmClient:       compute.NewVirtualMachinesClient(subscriptionID),
		nicClient:      network.NewInterfacesClient(subscriptionID),
		subnetsClient:  network.NewSubnetsClient(subscriptionID),
		vnetClient:     network.NewVirtualNetworksClient(subscriptionID),
	}

	authorizer := autorest.NewBearerAuthorizer(armSpt)
	c.groupsClient.Authorizer = authorizer
	c.vmClient.Authorizer = authorizer
	c.nicClient.Authorizer = authorizer
	c.subnetsClient.Authorizer = authorizer
	c.vnetClient.Authorizer = authorizer

	return c
}
