package extensionspec

// ExtensionSpec is the content of the ProviderConfig of the private-network extension object
type ExtensionSpec struct {
	// LbSubnetID     string `json:"lbSubnetID"`
	PrivateCluster bool `json:"privateCluster"`
}
