// The self-contained schema this plugin serves over Describe. There is NO schema-less
// plugin: NewMeta hands this to sdk.NewMeta, which builds the capability set from it.

#DeployFirecrackerPlugin: {
	// The venue this target deploys onto, and the kernel + root drive the VMM boots.
	venue?:     string
	kernel?:    string
	rootDrive?: string
	vcpus?:     int & >=1 & <=64
	memMiB?:    int & >=128

	...
}
