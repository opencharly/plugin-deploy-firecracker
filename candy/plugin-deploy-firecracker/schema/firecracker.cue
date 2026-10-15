// The self-contained schema this plugin serves over Describe. There is NO schema-less
// plugin: NewMeta hands this to sdk.NewMeta, which builds the capability set from it, and
// the host splices it onto the base schema at connect time. It imports nothing outside this
// module, by the per-plugin schema contract.
//
// ONE definition, declared once. This pair of files previously carried the same five fields
// twice — `#DeployFirecrackerPlugin` here and `#DeployFirecrackerInput` in
// deploy_firecracker.cue — a duplication with no consumer distinguishing them, removed by
// R2 in the same cutover. The surviving name is the one plugin.go's own comment names.

#DeployFirecrackerPlugin: {
	// The venue this deploy lands on. The microVM substrate is reached over ssh, and its
	// traits row (served by plugin-substrate) carries no exclusive_venue — the absence that
	// keeps the kernel from misrouting it into `charly vm build`/`create`.
	venue?: string

	// The root-drive artefact and the kernel: a firecracker guest boots from these two
	// rather than from a domain definition, so the provider requires both.
	//
	// `rootDrive` is camelCase HERE and the Go shape is derived FROM this file (plugin.go's
	// json tag). A tag of `root_drive` silently left RootDrive empty and made the provider
	// refuse every authored deploy — measured, and fixed by making the tag match this
	// definition rather than the other way round.
	rootDrive?: string
	kernel?:    string


	...
}
