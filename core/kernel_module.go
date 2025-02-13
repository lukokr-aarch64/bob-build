/*
 * Copyright 2018-2021 Arm Limited.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package core

import (
	"path/filepath"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"github.com/ARM-software/bob-build/internal/utils"
)

type KernelProps struct {
	// Linux kernel config options to emulate. These are passed to Kbuild in
	// the 'make' command-line, and set in the source code via EXTRA_CFLAGS
	Kbuild_options []string
	// Kernel modules which this module depends on
	Extra_symbols []string
	// Arguments to pass to kernel make invocation
	Make_args []string
	// Kernel directory location
	Kernel_dir *string
	// Compiler prefix for kernel build
	Kernel_cross_compile *string
	// Kernel target compiler
	Kernel_cc *string
	// Kernel host compiler
	Kernel_hostcc *string
	// Kernel linker
	Kernel_ld *string
	// Target triple when using clang as the compiler
	Kernel_clang_triple *string
}

func (k *KernelProps) processPaths(ctx blueprint.BaseModuleContext) {
	prefix := projectModuleDir(ctx)

	// join module dir with relative kernel dir
	kdir := proptools.String(k.Kernel_dir)
	if kdir != "" && !filepath.IsAbs(kdir) {
		kdir = filepath.Join(prefix, kdir)
		k.Kernel_dir = proptools.StringPtr(kdir)
	}
}

type kernelModule struct {
	moduleBase
	simpleOutputProducer
	Properties struct {
		Features
		CommonProps
		KernelProps
		Defaults []string
	}
}

// kernelModule supports the following functionality:
// * sharing properties via defaults
// * feature-specific properties
// * installation
// * module enabling/disabling
// * appending to aliases
var _ defaultable = (*kernelModule)(nil)
var _ featurable = (*kernelModule)(nil)
var _ installable = (*kernelModule)(nil)
var _ enableable = (*kernelModule)(nil)
var _ aliasable = (*kernelModule)(nil)

func (m *kernelModule) defaults() []string {
	return m.Properties.Defaults
}

func (m *kernelModule) defaultableProperties() []interface{} {
	return []interface{}{&m.Properties.CommonProps, &m.Properties.KernelProps}
}

func (m *kernelModule) featurableProperties() []interface{} {
	return []interface{}{&m.Properties.CommonProps, &m.Properties.KernelProps}
}

func (m *kernelModule) features() *Features {
	return &m.Properties.Features
}

func (m *kernelModule) outputName() string {
	return m.Name()
}

func (m *kernelModule) altName() string {
	return m.outputName()
}

func (m *kernelModule) altShortName() string {
	return m.altName()
}

func (m *kernelModule) shortName() string {
	return m.Name()
}

func (m *kernelModule) getEnableableProps() *EnableableProps {
	return &m.Properties.EnableableProps
}

func (m *kernelModule) getAliasList() []string {
	return m.Properties.getAliasList()
}

func (m *kernelModule) filesToInstall(ctx blueprint.BaseModuleContext) []string {
	return m.outputs()
}

func (m *kernelModule) getInstallableProps() *InstallableProps {
	return &m.Properties.InstallableProps
}

func (m *kernelModule) getInstallDepPhonyNames(ctx blueprint.ModuleContext) []string {
	return getShortNamesForDirectDepsWithTags(ctx, installDepTag, kernelModuleDepTag)
}

func (m *kernelModule) processPaths(ctx blueprint.BaseModuleContext, g generatorBackend) {
	m.Properties.CommonProps.processPaths(ctx, g)
	m.Properties.KernelProps.processPaths(ctx)
}

func (m *kernelModule) extraSymbolsModules(ctx blueprint.BaseModuleContext) (modules []*kernelModule) {
	ctx.VisitDirectDepsIf(
		func(m blueprint.Module) bool { return ctx.OtherModuleDependencyTag(m) == kernelModuleDepTag },
		func(m blueprint.Module) {
			if km, ok := m.(*kernelModule); ok {
				modules = append(modules, km)
			} else {
				utils.Die("invalid extra_symbols, %s not a kernel module", ctx.OtherModuleName(m))
			}
		})

	return
}

func (m *kernelModule) extraSymbolsFiles(ctx blueprint.BaseModuleContext) (files []string) {
	for _, mod := range m.extraSymbolsModules(ctx) {
		files = append(files, filepath.Join(mod.outputDir(), "Module.symvers"))
	}

	return
}

type kbuildArgs struct {
	KmodBuild          string
	ExtraIncludes      string
	ExtraCflags        string
	KernelDir          string
	KernelCrossCompile string
	KbuildOptions      string
	MakeArgs           string
	OutputModuleDir    string
	CCFlag             string
	HostCCFlag         string
	ClangTripleFlag    string
	LDFlag             string
}

func (a kbuildArgs) toDict() map[string]string {
	return map[string]string{
		"kmod_build":           a.KmodBuild,
		"extra_includes":       a.ExtraIncludes,
		"extra_cflags":         a.ExtraCflags,
		"kernel_dir":           a.KernelDir,
		"kernel_cross_compile": a.KernelCrossCompile,
		"kbuild_options":       a.KbuildOptions,
		"make_args":            a.MakeArgs,
		"output_module_dir":    a.OutputModuleDir,
		"cc_flag":              a.CCFlag,
		"hostcc_flag":          a.HostCCFlag,
		"clang_triple_flag":    a.ClangTripleFlag,
		"ld_flag":              a.LDFlag,
	}
}

func (m *kernelModule) generateKbuildArgs(ctx blueprint.BaseModuleContext) kbuildArgs {
	var extraIncludePaths []string

	g := getBackend(ctx)

	extraCflags := m.Properties.Cflags

	for _, includeDir := range m.Properties.IncludeDirsProps.Local_include_dirs {
		includeDir = "-I" + getBackendPathInSourceDir(g, includeDir)
		extraIncludePaths = append(extraIncludePaths, includeDir)
	}

	for _, includeDir := range m.Properties.IncludeDirsProps.Include_dirs {
		includeDir = "-I" + includeDir
		extraIncludePaths = append(extraIncludePaths, includeDir)
	}

	kmodBuild := getBackendPathInBobScriptsDir(g, "kmod_build.py")
	kdir := proptools.String(m.Properties.KernelProps.Kernel_dir)
	if kdir != "" && !filepath.IsAbs(kdir) {
		kdir = getBackendPathInSourceDir(g, kdir)
	}

	kbuildOptions := ""
	if len(m.Properties.KernelProps.Kbuild_options) > 0 {
		kbuildOptions = "--kbuild-options " + strings.Join(m.Properties.KernelProps.Kbuild_options, " ")
	}

	hostToolchain := proptools.String(m.Properties.KernelProps.Kernel_hostcc)
	if hostToolchain != "" {
		hostToolchain = "--hostcc " + hostToolchain
	}

	kernelToolchain := proptools.String(m.Properties.KernelProps.Kernel_cc)
	if kernelToolchain != "" {
		kernelToolchain = "--cc " + kernelToolchain
	}

	clangTriple := proptools.String(m.Properties.KernelProps.Kernel_clang_triple)
	if clangTriple != "" {
		clangTriple = "--clang-triple " + clangTriple
	}

	ld := proptools.String(m.Properties.KernelProps.Kernel_ld)
	if ld != "" {
		ld = "--ld " + ld
	}

	return kbuildArgs{
		KmodBuild:          kmodBuild,
		ExtraIncludes:      strings.Join(extraIncludePaths, " "),
		ExtraCflags:        strings.Join(extraCflags, " "),
		KernelDir:          kdir,
		KernelCrossCompile: proptools.String(m.Properties.KernelProps.Kernel_cross_compile),
		KbuildOptions:      kbuildOptions,
		MakeArgs:           strings.Join(m.Properties.KernelProps.Make_args, " "),
		// The kernel module builder replicates the out-of-tree module's source tree structure.
		// The kernel module will be at its equivalent position in the output tree.
		OutputModuleDir: filepath.Join(m.outputDir(), projectModuleDir(ctx)),
		CCFlag:          kernelToolchain,
		HostCCFlag:      hostToolchain,
		LDFlag:          ld,
		ClangTripleFlag: clangTriple,
	}
}

func (m *kernelModule) GenerateBuildActions(ctx blueprint.ModuleContext) {
	if isEnabled(m) {
		getBackend(ctx).kernelModuleActions(m, ctx)
	}
}

func kernelModuleFactory(config *bobConfig) (blueprint.Module, []interface{}) {
	module := &kernelModule{}

	module.Properties.Features.Init(&config.Properties, CommonProps{}, KernelProps{})

	return module, []interface{}{&module.Properties, &module.SimpleName.Properties}
}
