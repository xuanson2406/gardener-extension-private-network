package extensionspec

// ExtensionSpec is the content of the ProviderConfig of the private-network extension object
type ExtensionSpec struct {
	PrivateCluster bool     `json:"privateCluster"`
	AlowCIDR       []string `json:"allowCIDR"`
}
