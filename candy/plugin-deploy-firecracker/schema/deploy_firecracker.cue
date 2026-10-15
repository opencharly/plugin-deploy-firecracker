// The served schema for deploy:firecracker — the input shape the kernel hands this
// deploy-target provider alongside the deployment's InstallPlan views.
//
// Self-contained by the per-plugin schema contract: it imports nothing outside this
// module, so the host can splice it onto the base schema at connect time.

#DeployFirecrackerInput: {
	// The venue this deploy lands on. The microVM substrate is reached over ssh, and
	// its traits row (served by plugin-substrate) carries no exclusive_venue — the
	// absence that keeps the kernel from misrouting it into `charly vm build`/`create`.
	venue?: string

	// The root-drive artefact and the kernel: a firecracker guest boots from these two
	// rather than from a domain definition.
	rootDrive?: string
	kernel?: string

	// Optional machine geometry. Absent means the venue's own default applies.
	vcpus?:  int & >=1 & <=64
	memMiB?: int & >=128

	...
}
