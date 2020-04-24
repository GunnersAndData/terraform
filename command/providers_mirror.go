package command

import (
	"fmt"
	"path/filepath"

	"github.com/apparentlymart/go-versions/versions"
	"github.com/hashicorp/terraform/internal/getproviders"
	"github.com/hashicorp/terraform/tfdiags"
)

// ProvidersMirrorCommand is a Command implementation that implements the
// "terraform providers mirror" command, which populates a directory with
// local copies of provider plugins needed by the current configuration so
// that the mirror can be used to work offline, or similar.
type ProvidersMirrorCommand struct {
	Meta
}

func (c *ProvidersMirrorCommand) Synopsis() string {
	return "Mirrors the provider plugins needed for the current configuration"
}

func (c *ProvidersMirrorCommand) Run(args []string) int {
	args = c.Meta.process(args)
	cmdFlags := c.Meta.defaultFlagSet("providers mirror")
	var optPlatforms FlagStringSlice
	cmdFlags.Var(&optPlatforms, "platform", "target platform")
	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		c.Ui.Error(fmt.Sprintf("Error parsing command-line flags: %s\n", err.Error()))
		return 1
	}

	var diags tfdiags.Diagnostics

	args = cmdFlags.Args()
	if len(args) != 1 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"No output directory specified",
			"The providers mirror command requires an output directory as a command-line argument.",
		))
		c.showDiagnostics(diags)
		return 1
	}
	outputDir := args[0]

	var platforms []getproviders.Platform
	if len(optPlatforms) == 0 {
		platforms = []getproviders.Platform{getproviders.CurrentPlatform}
	} else {
		platforms = make([]getproviders.Platform, 0, len(optPlatforms))
		for _, platformStr := range optPlatforms {
			platform, err := getproviders.ParsePlatform(platformStr)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Invalid target platform",
					fmt.Sprintf("The string %q given in the -platform option is not a valid target platform: %s.", platformStr, err),
				))
				continue
			}
			platforms = append(platforms, platform)
		}
	}

	config, confDiags := c.loadConfig(".")
	diags = diags.Append(confDiags)
	reqs, moreDiags := config.ProviderRequirements()
	diags = diags.Append(moreDiags)

	// If we have any error diagnostics already then we won't proceed further.
	if diags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	// Unlike other commands, this command always consults the origin registry
	// for every provider so that it can be used to update a local mirror
	// directory without needing to first disable that local mirror
	// in the CLI configuration.
	source := getproviders.NewMemoizeSource(
		getproviders.NewRegistrySource(c.Services),
	)

	for provider, constraints := range reqs {
		if provider.IsBuiltIn() {
			c.Ui.Output(fmt.Sprintf("- Skipping %s because it is built in to Terraform CLI", provider.ForDisplay()))
			continue
		}
		constraintsStr := getproviders.VersionConstraintsString(constraints)
		c.Ui.Output(fmt.Sprintf("- Mirroring %s...", provider.ForDisplay()))
		// First we'll look for the latest version that matches the given
		// constraint, which we'll then try to mirror for each target platform.
		acceptable := versions.MeetingConstraints(constraints)
		avail, err := source.AvailableVersions(provider)
		candidates := avail.Filter(acceptable)
		if err == nil && len(candidates) == 0 {
			err = fmt.Errorf("no releases match the given constraints %s", constraintsStr)
		}
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Provider not available",
				fmt.Sprintf("Failed to download %s from its origin registry: %s.", provider.String(), err),
			))
			continue
		}
		selected := candidates.Newest()
		if len(constraintsStr) > 0 {
			c.Ui.Output(fmt.Sprintf("  - Selected v%s to meet constraints %s", selected.String(), constraintsStr))
		} else {
			c.Ui.Output(fmt.Sprintf("  - Selected v%s with no constraints", selected.String()))
		}
		for _, platform := range platforms {
			c.Ui.Output(fmt.Sprintf("  - Downloading package for %s...", platform.String()))
			meta, err := source.PackageMeta(provider, selected, platform)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Provider release not available",
					fmt.Sprintf("Failed to download %s v%s for %s: %s.", provider.String(), selected.String(), platform.String(), err),
				))
				continue
			}
			url, ok := meta.Location.(getproviders.PackageHTTPURL)
			if !ok {
				// We don't expect to get non-HTTP locations here because we're
				// using the registry source, so this seems like a bug in the
				// registry source.
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Provider release not available",
					fmt.Sprintf("Failed to download %s v%s for %s: Terraform's provider registry client returned unexpected location type %T. This is a bug in Terraform.", provider.String(), selected.String(), platform.String(), meta.Location),
				))
				continue
			}
			// targetPath is the path where we ultimately want to place the
			// downloaded archive, but we'll place it initially at stagingPath
			// so we can verify its checksums and signatures before making
			// it discoverable to mirror clients. (stagingPath intentionally
			// does not follow the filesystem mirror file naming convention.)
			targetPath := meta.PackedFilePath(outputDir)
			stagingPath := filepath.Join(filepath.Dir(targetPath), "."+filepath.Base(targetPath))
			fmt.Printf("TODO: Download %s to %s via %s\n", url, targetPath, stagingPath)
		}
	}

	c.showDiagnostics(diags)
	if diags.HasErrors() {
		return 1
	}
	return 0
}

func (c *ProvidersMirrorCommand) Help() string {
	return `
Usage: terraform providers mirror [options] <target-dir>

  Populates a local directory with copies of the provider plugins needed for
  the current configuration, so that the directory can be used either directly
  as a filesystem mirror or as the basis for a network mirror and thus obtain
  those providers without access to their origin registries in future.

  The mirror directory will contain JSON index files that can be published
  along with the mirrored packages on a static HTTP file server to produce
  a network mirror. Those index files will be ignored if the directory is
  used instead as a local filesystem mirror.

Options:

  -platform=os_arch  Choose which target platform to build a mirror for.
                     By default Terraform will obtain plugin packages
                     suitable for the platform where you run this command.
                     Use this flag multiple times to include packages for
                     multiple target systems.

                     Target names consist of an operating system and a CPU
                     architecture. For example, "linux_amd64" selects the
                     Linux operating system running on an AMD64 or x86_64
                     CPU. Each provider is available only for a limited
                     set of target platforms.
`
}
