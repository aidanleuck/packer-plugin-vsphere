// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:generate packer-sdc mapstructure-to-hcl2 -type Config

package vsphere_template

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	vsphere "github.com/hashicorp/packer-plugin-vsphere/builder/vsphere/common"
	vspherepost "github.com/hashicorp/packer-plugin-vsphere/post-processor/vsphere"
	"github.com/vmware/govmomi"
)

const (
	// BuilderId for the local artifacts
	BuilderIdESX = "mitchellh.vmware-esx"

	ArtifactConfFormat         = "artifact.conf.format"
	ArtifactConfKeepRegistered = "artifact.conf.keep_registered"
	ArtifactConfSkipExport     = "artifact.conf.skip_export"
)

var builtins = map[string]string{
	vspherepost.BuilderId:            "vmware",
	BuilderIdESX:                     "vmware",
	vsphere.BuilderId:                "vsphere",
	"packer.post-processor.artifice": "artifice",
}

type Config struct {
	common.PackerConfig `mapstructure:",squash"`
	Host                string         `mapstructure:"host"`
	Insecure            bool           `mapstructure:"insecure"`
	Username            string         `mapstructure:"username"`
	Password            string         `mapstructure:"password"`
	Datacenter          string         `mapstructure:"datacenter"`
	Folder              string         `mapstructure:"folder"`
	SnapshotEnable      bool           `mapstructure:"snapshot_enable"`
	SnapshotName        string         `mapstructure:"snapshot_name"`
	SnapshotDescription string         `mapstructure:"snapshot_description"`
	ReregisterVM        config.Trilean `mapstructure:"reregister_vm"`

	ctx interpolate.Context
}

type PostProcessor struct {
	config Config
	url    *url.URL
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         vsphere.BuilderId,
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{},
		},
	}, raws...)

	if err != nil {
		return err
	}

	errs := new(packersdk.MultiError)
	vc := map[string]*string{
		"host":     &p.config.Host,
		"username": &p.config.Username,
		"password": &p.config.Password,
	}

	for key, ptr := range vc {
		if *ptr == "" {
			errs = packersdk.MultiErrorAppend(
				errs, fmt.Errorf("%s must be set", key))
		}
	}

	sdk, err := url.Parse(fmt.Sprintf("https://%v/sdk", p.config.Host))
	if err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("Error invalid vSphere sdk endpoint: %s", err))
		return errs
	}

	sdk.User = url.UserPassword(p.config.Username, p.config.Password)
	p.url = sdk

	if len(errs.Errors) > 0 {
		return errs
	}
	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, artifact packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	if _, ok := builtins[artifact.BuilderId()]; !ok {
		return nil, false, false, fmt.Errorf("The Packer vSphere Template post-processor "+
			"can only take an artifact from the VMware-iso builder, built on "+
			"ESXi (i.e. remote) or an artifact from the vSphere post-processor. "+
			"Artifact type %s does not fit this requirement", artifact.BuilderId())
	}

	f := artifact.State(ArtifactConfFormat)
	k := artifact.State(ArtifactConfKeepRegistered)
	s := artifact.State(ArtifactConfSkipExport)

	if f != "" && k != "true" && s == "false" {
		return nil, false, false, errors.New("To use this post-processor with exporting behavior you need set keep_registered as true")
	}

	// In some occasions the VM state is powered on and if we immediately try to mark as template
	// (after the ESXi creates it) it will fail. If vSphere is given a few seconds this behavior doesn't reappear.
	ui.Message("Waiting 10s for VMware vSphere to start")
	time.Sleep(10 * time.Second)
	c, err := govmomi.NewClient(context.Background(), p.url, p.config.Insecure)
	if err != nil {
		return nil, false, false, fmt.Errorf("Error connecting to vSphere: %s", err)
	}

	defer p.Logout(c)

	state := new(multistep.BasicStateBag)
	state.Put("ui", ui)
	state.Put("client", c)

	steps := []multistep.Step{
		&stepChooseDatacenter{
			Datacenter: p.config.Datacenter,
		},
		&stepCreateFolder{
			Folder: p.config.Folder,
		},
		NewStepCreateSnapshot(artifact, p),
		NewStepMarkAsTemplate(artifact, p),
	}
	runner := commonsteps.NewRunnerWithPauseFn(steps, p.config.PackerConfig, ui, state)
	runner.Run(ctx, state)
	if rawErr, ok := state.GetOk("error"); ok {
		return nil, false, false, rawErr.(error)
	}
	return artifact, true, true, nil
}

func (p *PostProcessor) Logout(c *govmomi.Client) {
	_ = c.Logout(context.Background())
}
