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
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"github.com/ARM-software/bob-build/internal/ccflags"
	"github.com/ARM-software/bob-build/internal/escape"
	"github.com/ARM-software/bob-build/internal/fileutils"
	"github.com/ARM-software/bob-build/internal/utils"
)

type binType int

const (
	binTypeStatic     binType = binType(0)
	binTypeShared     binType = binType(1)
	binTypeExecutable binType = binType(2)
)

var (
	androidLock sync.Mutex
)

type androidMkGenerator struct {
	toolchainSet
}

/* Compile time checks for interfaces that must be implemented by androidMkGenerator */
var _ generatorBackend = (*androidMkGenerator)(nil)

func (g *androidMkGenerator) escapeFlag(s string) string {
	return escape.MakefileAndShellEscape(s)
}

func androidMkWriteString(ctx blueprint.ModuleContext, name string, sb *strings.Builder) {
	filename := getPathInBuildDir(name + ".inc")
	err := fileutils.WriteIfChanged(filename, sb)
	if err != nil {
		utils.Die("%v", err.Error())
	}
}

func writeListAssignment(sb *strings.Builder, varname string, entries []string) {
	if len(entries) > 0 {
		sb.WriteString(varname + " := " + strings.Join(entries, " ") + "\n")
	}
}

func newlineSeparatedList(list []string) string {
	return " \\\n    " + strings.Join(list, " \\\n    ") + "\n"
}

var (
	classes = []string{
		"STATIC_LIBRARIES",
		"SHARED_LIBRARIES",
		"EXECUTABLES",
	}

	rulePrefix = map[tgtType]string{
		tgtTypeTarget: "BUILD_",
		tgtTypeHost:   "BUILD_HOST_",
	}

	ruleSuffix = []string{
		"STATIC_LIBRARY",
		"SHARED_LIBRARY",
		"EXECUTABLE",
	}
)

func specifyCompilerStandard(varname string, flags ...[]string) (line string) {
	if std := ccflags.GetCompilerStandard(flags...); std != "" {
		line = varname + ":=" + std + "\n"
	}
	return
}

func specifyArmMode(flags ...[]string) (line string) {
	armMode, err := ccflags.GetArmMode(flags...)
	if err != nil {
		panic(err)
	}

	if armMode != "" {
		line = "LOCAL_ARM_MODE:=" + armMode + "\n"
	}
	return
}

// Identifies if a module links to a generated library. Generated
// libraries only support a single architecture
func linksToGeneratedLibrary(ctx blueprint.ModuleContext) bool {
	seenGeneratedLib := false

	ctx.WalkDeps(func(dep, parent blueprint.Module) bool {

		// Only consider dependencies that get linked
		tag := ctx.OtherModuleDependencyTag(dep)
		if tag == staticDepTag ||
			tag == sharedDepTag ||
			tag == wholeStaticDepTag {

			_, staticLib := dep.(*generateStaticLibrary)
			_, sharedLib := dep.(*generateSharedLibrary)
			if sharedLib || staticLib {
				// We depend on a generated library
				seenGeneratedLib = true

				// No need to continue walking
				return false
			}

			// Keep walking this part of the tree
			return true
		}

		return false
	})

	return seenGeneratedLib
}

// This function generates the Android make fragment to build static
// libraries, shared libraries and executables. It's evolved over time
// and needs to be refactored to use interfaces better.
func androidLibraryBuildAction(sb *strings.Builder, mod blueprint.Module, ctx blueprint.ModuleContext, tcs toolchainSet) {
	var bt binType
	var m *library
	var libname string

	switch real := mod.(type) {
	case *staticLibrary:
		bt = binTypeStatic
		m = &real.library
		libname = real.outputFileName()
	case *sharedLibrary:
		bt = binTypeShared
		m = &real.library
		libname = real.outputFileName()
	case *binary:
		bt = binTypeExecutable
		m = &real.library
		libname = real.outputFileName()
	default:
		utils.Die("Unexpected module type %T", real)
	}

	if m.Properties.Build_wrapper != nil {
		utils.Die("build_wrapper not supported on Android")
	}

	// Calculate and record outputs
	m.outs = []string{filepath.Join(m.outputDir(), libname)}

	sb.WriteString("##########################\ninclude $(CLEAR_VARS)\n\n")
	sb.WriteString("LOCAL_MODULE:=" + m.altName() + "\n")
	sb.WriteString("LOCAL_MODULE_CLASS:=" + classes[bt] + "\n\n")

	// The order we want is  local_include_dirs, export_local_include_dirs,
	//                       include_dirs, export_include_dirs
	// This is because include and export include should be system headers
	includes := utils.PrefixDirs(m.Properties.Local_include_dirs, "$(LOCAL_PATH)")
	includes = append(includes, utils.PrefixDirs(m.Properties.Export_local_include_dirs, "$(LOCAL_PATH)")...)
	includes = append(includes, m.Properties.Include_dirs...)
	includes = append(includes, m.Properties.Export_include_dirs...)

	exportIncludeDirs := utils.NewStringSlice(m.Properties.Export_include_dirs, utils.PrefixDirs(m.Properties.Export_local_include_dirs, "$(LOCAL_PATH)"))

	// Handle generated headers
	headerDirs, headerOutputs := m.GetGeneratedHeaders(ctx)
	includes = append(includes, headerDirs...)
	additionalDeps := headerOutputs

	// Handle generated sources
	for _, module := range m.getAllGeneratedSourceModules(ctx) {
		// LOCAL_GENERATED_SOURCES is used to name target generated as
		// part of this module which we also link into a library. The
		// generated sources are automatically added to the
		// library. Unfortunately, we've generated the sources in a
		// separate module...
		//
		// To compile a file generated in another module we could try
		// and explicitly list the file in that modules directory in
		// LOCAL_SRCS. However Android make won't work with LOCAL_SRCS
		// outside the source tree, so we can't do that.
		//
		// Therefore we use LOCAL_GENERATED_SOURCES and copy the files
		// generated in the other module into this module.
		//
		// An alternative would be to avoid the separate source
		// generation module and do it as part of this module.

		sources := "$(" + module + "_OUTPUTS)"
		sourcesDir := "$(" + module + "_GEN_DIR)"

		localSourceExpr := "$(subst " + sourcesDir + ", $(local-generated-sources-dir), " + sources + ")"
		localSources := "$(" + m.altName() + "_" + module + "_SRCS)"

		sb.WriteString(m.altName() + "_" + module + "_SRCS:=" + localSourceExpr + "\n")
		sb.WriteString("LOCAL_GENERATED_SOURCES+=" + localSources + "\n")

		// Copy rule. Use a static pattern to avoid running the command for each file
		sb.WriteString(localSources + ": $(local-generated-sources-dir)" + "/%: " + sourcesDir + "/%\n")
		sb.WriteString("\tcp $< $@\n\n")
	}

	if getConfig(ctx).Properties.GetBool("target_toolchain_clang") {
		sb.WriteString("LOCAL_CLANG := true\n")
	} else {
		sb.WriteString("LOCAL_CLANG := false\n")
	}
	srcs := m.Properties.getSources(ctx)

	// Remove sources which are not compiled
	nonCompiledDeps := utils.Filter(utils.IsNotCompilableSource, srcs)
	srcs = utils.Filter(utils.IsCompilableSource, srcs)

	writeListAssignment(sb, "LOCAL_SRC_FILES", srcs)

	versionScript := m.getVersionScript(ctx)
	if bt == binTypeShared || bt == binTypeExecutable {
		if versionScript != nil {
			additionalDeps = append(additionalDeps, *versionScript)
		}
	}

	additionalDeps = append(additionalDeps, utils.PrefixDirs(nonCompiledDeps, "$(LOCAL_PATH)")...)
	writeListAssignment(sb, "LOCAL_ADDITIONAL_DEPENDENCIES", additionalDeps)
	writeListAssignment(sb, "LOCAL_C_INCLUDES", includes)

	cflagsList := utils.NewStringSlice(m.Properties.Cflags, m.Properties.Export_cflags)
	_, _, exportedCflags := m.GetExportedVariables(ctx)
	cflagsList = append(cflagsList, exportedCflags...)
	writeListAssignment(sb, "LOCAL_CFLAGS",
		utils.Filter(ccflags.AndroidCompileFlags, cflagsList))
	writeListAssignment(sb, "LOCAL_CPPFLAGS",
		utils.Filter(ccflags.AndroidCompileFlags, m.Properties.Cxxflags))
	writeListAssignment(sb, "LOCAL_CONLYFLAGS",
		utils.Filter(ccflags.AndroidCompileFlags, m.Properties.Conlyflags))

	// Setup module C/C++ standard if requested. Note that this only affects Android O and later.
	sb.WriteString(specifyCompilerStandard("LOCAL_C_STD", cflagsList, m.Properties.Conlyflags))
	sb.WriteString(specifyCompilerStandard("LOCAL_CPP_STD", cflagsList, m.Properties.Cxxflags))

	// Setup ARM mode if needed
	sb.WriteString(specifyArmMode(cflagsList, m.Properties.Conlyflags, m.Properties.Cxxflags))

	// convert Shared_libs, Resolved_static_libs, and Whole_static_libs
	// to Android module names rather than Bob module names
	sharedLibs := androidModuleNames(m.Properties.Shared_libs)
	staticLibs := androidModuleNames(m.Properties.ResolvedStaticLibs)
	wholeStaticLibs := androidModuleNames(m.Properties.Whole_static_libs)
	exportHeaderLibs := androidModuleNames(m.Properties.Export_header_libs)
	headerLibs := append(androidModuleNames(m.Properties.Header_libs), exportHeaderLibs...)

	writeListAssignment(sb, "LOCAL_SHARED_LIBRARIES", sharedLibs)
	writeListAssignment(sb, "LOCAL_STATIC_LIBRARIES", staticLibs)
	writeListAssignment(sb, "LOCAL_WHOLE_STATIC_LIBRARIES", wholeStaticLibs)
	writeListAssignment(sb, "LOCAL_HEADER_LIBRARIES", headerLibs)

	reexportShared := []string{}
	reexportStatic := []string{}
	reexportHeaders := exportHeaderLibs
	for _, lib := range androidModuleNames(m.Properties.Reexport_libs) {
		if utils.Contains(sharedLibs, lib) {
			reexportShared = append(reexportShared, lib)
		} else if utils.Contains(staticLibs, lib) {
			reexportStatic = append(reexportStatic, lib)
		} else if utils.Contains(headerLibs, lib) {
			reexportHeaders = append(reexportHeaders, lib)
		}
	}

	writeListAssignment(sb, "LOCAL_EXPORT_SHARED_LIBRARY_HEADERS", reexportShared)
	writeListAssignment(sb, "LOCAL_EXPORT_STATIC_LIBRARY_HEADERS", reexportStatic)
	writeListAssignment(sb, "LOCAL_EXPORT_HEADER_LIBRARY_HEADERS", reexportHeaders)

	writeListAssignment(sb, "LOCAL_MODULE_TAGS", m.Properties.Tags)
	writeListAssignment(sb, "LOCAL_EXPORT_C_INCLUDE_DIRS", exportIncludeDirs)
	if m.Properties.isProprietary() {
		sb.WriteString("LOCAL_MODULE_OWNER := " + proptools.String(m.Properties.Owner) + "\n")
		sb.WriteString("LOCAL_PROPRIETARY_MODULE := true\n")
	}
	if strlib, ok := mod.(stripable); ok && strlib.strip() {
		sb.WriteString("LOCAL_STRIP_MODULE := true\n")
	}

	tgt := m.Properties.TargetType

	var tc toolchain
	if tgt == tgtTypeTarget {
		tc = tcs.target
	} else {
		tc = tcs.host
	}

	// Can't see a way to wrap a particular library in -Wl in link flags on android, so specify
	// -Wl,--copy-dt-needed-entries across the lot
	hasForwardingLib := false
	copydtneeded := ""
	ctx.VisitDirectDepsIf(
		func(p blueprint.Module) bool { return ctx.OtherModuleDependencyTag(p) == sharedDepTag },
		func(p blueprint.Module) {
			if sl, ok := p.(*sharedLibrary); ok {
				b := &sl.library.Properties.Build
				if b.isForwardingSharedLibrary() {
					hasForwardingLib = true
				}
			} else if _, ok := p.(*generateSharedLibrary); ok {
				// Generated forwarding lib not supported
			} else if _, ok := p.(*externalLib); ok {
				// External libraries are never forwarding libraries
			} else {
				utils.Die("%s is not a shared library", ctx.OtherModuleName(p))
			}
		})
	if hasForwardingLib {
		copydtneeded = "-fuse-ld=bfd " + tc.getLinker().keepSharedLibraryTransitivity()
	}

	// Handle installation
	installBase, installRel, ok := getAndroidInstallPath(&m.Properties.InstallableProps)

	// Only setup multilib for target modules.
	// Normally this should only apply to target libraries, but we
	// also do multilib target binaries to allow creation of test
	// binaries in both modes.
	// All test binaries will be installable.
	isMultiLib := (tgt == tgtTypeTarget) &&
		((bt == binTypeShared) || (bt == binTypeStatic) || ok)

	// Disable multilib if this module depends on generated libraries
	// (which can't support multilib)
	if isMultiLib && linksToGeneratedLibrary(ctx) {
		isMultiLib = false
	}

	if ok {
		if m.Properties.Post_install_cmd != nil {
			// Setup args like we do for bob_generated_*
			args := map[string]string{}
			if m.Properties.Post_install_tool != nil {
				args["tool"] = *m.Properties.Post_install_tool
			}
			args["out"] = "$(LOCAL_INSTALLED_MODULE)"

			// We can't use target specific variables in make due to
			// the way LOCAL_POST_INSTALL_CMD is
			// implemented. Therefore expand all variable use here.
			cmd := strings.Replace(*m.Properties.Post_install_cmd, "${args}",
				strings.Join(m.Properties.Post_install_args, " "), -1)
			for key, value := range args {
				cmd = strings.Replace(cmd, "${"+key+"}", value, -1)
			}

			// Intentionally using a recursively expanded variable. We
			// don't want LOCAL_INSTALLED_MODULE expanded now, but
			// when it is used in base_rules.mk
			sb.WriteString("LOCAL_POST_INSTALL_CMD=" + cmd + "\n")
		}

		if bt == binTypeExecutable {
			if isMultiLib {
				// For executables we need to be clear about where to
				// install both 32 and 64 bit versions of the
				// binaries. This is done by appending `64` to the install dir.
				// However, there are not separate variables for the 32 and 64-bit
				// relative paths, so ignore LOCAL_MODULE_RELATIVE_PATH and put the
				// relative_install_path in LOCAL_MODULE_PATH_[32|64] instead.
				//
				// LOCAL_UNSTRIPPED_PATH does not need to be set
				fullInstallPath := filepath.Join(installBase, installRel)
				sb.WriteString("LOCAL_MODULE_PATH_32:=" + fullInstallPath + "\n")
				sb.WriteString("LOCAL_MODULE_PATH_64:=" + fullInstallPath + "64\n")
			} else {
				sb.WriteString("LOCAL_MODULE_PATH:=" + installBase + "\n")
				sb.WriteString("LOCAL_MODULE_RELATIVE_PATH:=" + installRel + "\n")

				// When LOCAL_MODULE_PATH is specified, you need to
				// specify LOCAL_UNSTRIPPED_PATH too
				if tgt == tgtTypeTarget {
					// Unstripped executables only generated for target
					sb.WriteString("LOCAL_UNSTRIPPED_PATH:=$(TARGET_OUT_EXECUTABLES_UNSTRIPPED)\n")
				}
			}
		} else {
			// You can't specify an explicit install dir for
			// libraries, so we can only control LOCAL_MODULE_RELATIVE_PATH
			sb.WriteString("LOCAL_MODULE_RELATIVE_PATH:=" + installRel + "\n")
		}

		requiredModuleNames := m.getInstallDepPhonyNames(ctx)
		if len(requiredModuleNames) > 0 {
			for i, v := range requiredModuleNames {
				requiredModuleNames[i] = androidModuleName(v)
			}
			sb.WriteString("LOCAL_REQUIRED_MODULES:=" + newlineSeparatedList(requiredModuleNames))
		}
	} else {
		// Only disable installation on the target, because host
		// libraries need to be installed to be used by the build.
		//
		// Target shared libraries do not need an explicit installation
		// location, but cannot be uninstallable, or the multilib paths
		// will conflict, resulting in the same location being used for
		// both 32 and 64-bit versions.
		if tgt == tgtTypeTarget && bt != binTypeShared {
			sb.WriteString("LOCAL_UNINSTALLABLE_MODULE:=true\n")
		}
	}

	ldflags := utils.Filter(ccflags.AndroidLinkFlags, m.Properties.Ldflags)
	ldflags = append(ldflags, copydtneeded)

	if (bt == binTypeShared || bt == binTypeExecutable) && versionScript != nil {
		ldflags = append(ldflags, tc.getLinker().setVersionScript(*versionScript))
	}

	if isMultiLib {
		sb.WriteString("LOCAL_MULTILIB:=both\n")
		writeListAssignment(sb, "LOCAL_LDFLAGS_32", ldflags)
		writeListAssignment(sb, "LOCAL_LDFLAGS_64", ldflags)
	} else {
		writeListAssignment(sb, "LOCAL_LDFLAGS", ldflags)
	}

	if tgt == tgtTypeTarget {
		writeListAssignment(sb, "LOCAL_LDLIBS", m.Properties.Ldlibs)
	} else {
		writeListAssignment(sb, "LOCAL_LDLIBS_$(HOST_OS)", m.Properties.Ldlibs)
	}
	sb.WriteString("\ninclude $(" + rulePrefix[tgt] + ruleSuffix[bt] + ")\n")

	androidMkWriteString(ctx, m.altShortName(), sb)
}

func (g *androidMkGenerator) staticActions(m *staticLibrary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		m.outputdir = g.staticLibOutputDir(m)
		androidLibraryBuildAction(sb, m, ctx, g.toolchainSet)
	}
}

func (g *androidMkGenerator) sharedActions(m *sharedLibrary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		m.outputdir = g.sharedLibOutputDir(m)
		androidLibraryBuildAction(sb, m, ctx, g.toolchainSet)
	}
}

func (g *androidMkGenerator) binaryActions(m *binary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		m.outputdir = g.binaryOutputDir(m)
		androidLibraryBuildAction(sb, m, ctx, g.toolchainSet)
	}
}

func (*androidMkGenerator) declareAlias(sb *strings.Builder, name string, srcs []string) {
	sb.WriteString("\ninclude $(CLEAR_VARS)\n\n")
	sb.WriteString("LOCAL_MODULE := " + name + "\n")

	sb.WriteString("LOCAL_REQUIRED_MODULES :=" + newlineSeparatedList(srcs))

	sb.WriteString("\n.PHONY: " + name + "\n")
	sb.WriteString(name + ": $(LOCAL_REQUIRED_MODULES)\n\n")

	sb.WriteString("include $(base_rules.mk)\n")
}

func (g *androidMkGenerator) aliasActions(m *alias, ctx blueprint.ModuleContext) {
	sb := &strings.Builder{}
	g.declareAlias(sb, m.Name(), m.Properties.Srcs)
	androidMkWriteString(ctx, m.Name(), sb)
}

func pathToModuleName(path string) string {
	path = strings.Replace(path, "/", "__", -1)
	path = strings.Replace(path, ".", "_", -1)
	path = strings.Replace(path, "(", "_", -1)
	path = strings.Replace(path, "$", "_", -1)
	path = strings.Replace(path, ")", "_", -1)
	return path
}

func (g *androidMkGenerator) resourceActions(m *resource, ctx blueprint.ModuleContext) {
	if !enabledAndRequired(m) {
		return
	}
	sb := &strings.Builder{}

	installBase, installRel, ok := getAndroidInstallPath(&m.Properties.InstallableProps)
	if !ok {
		androidMkWriteString(ctx, m.altShortName(), sb)
		return
	}

	filesToInstall := m.filesToInstall(ctx)
	requiredModuleNames := m.getInstallDepPhonyNames(ctx)

	for _, file := range filesToInstall {
		moduleName := pathToModuleName(file)
		requiredModuleNames = append(requiredModuleNames, moduleName)

		sb.WriteString("\ninclude $(CLEAR_VARS)\n\n")
		sb.WriteString("LOCAL_MODULE := " + moduleName + "\n")
		sb.WriteString("LOCAL_INSTALLED_MODULE_STEM := " + filepath.Base(file) + "\n")
		sb.WriteString("LOCAL_MODULE_CLASS := ETC\n")
		sb.WriteString("LOCAL_MODULE_PATH := " + installBase + "\n")
		sb.WriteString("LOCAL_MODULE_RELATIVE_PATH := " + installRel + "\n")
		writeListAssignment(sb, "LOCAL_MODULE_TAGS", m.Properties.Tags)
		sb.WriteString("LOCAL_SRC_FILES := " + file + "\n")
		if m.Properties.isProprietary() {
			sb.WriteString("LOCAL_MODULE_OWNER := " + proptools.String(m.Properties.Owner) + "\n")
			sb.WriteString("LOCAL_PROPRIETARY_MODULE := true\n")
		}
		sb.WriteString("\ninclude $(BUILD_PREBUILT)\n")
	}

	g.declareAlias(sb, m.Name(), requiredModuleNames)

	androidMkWriteString(ctx, m.altShortName(), sb)
}

func (g *androidMkGenerator) sourceDir() string {
	return "$(LOCAL_PATH)"
}

func (g *androidMkGenerator) buildDir() string {
	return "$(BOB_ANDROIDMK_DIR)"
}

func (g *androidMkGenerator) bobScriptsDir() string {
	srcToScripts, _ := filepath.Rel(getSourceDir(), getBobScriptsDir())
	return filepath.Join(g.sourceDir(), srcToScripts)
}

func outputDirVarName(m *generateCommon) string {
	return m.Name() + "_GEN_DIR"
}

func (g *androidMkGenerator) sourceOutputDir(m *generateCommon) string {
	if m.Properties.Target != tgtTypeHost {
		return filepath.Join("$(TARGET_OUT_GEN)", "STATIC_LIBRARIES", m.Name())
	}
	return filepath.Join("$(HOST_OUT_GEN)", "STATIC_LIBRARIES", m.Name())
}

func outputsVarName(m *generateCommon) string {
	return m.Name() + "_OUTPUTS"
}

func (g *androidMkGenerator) outputs(m *generateCommon) string {
	return "$(" + outputsVarName(m) + ")"
}

// Setup rule to create export_includes
func writeExportIncludeMkText(sb *strings.Builder, moduleName string) {
	// The following makefile snippets are based on Android makefiles from AOSP
	//  See aosp/build/core/make/prebuilt_internal.mk

	sb.WriteString("export_includes:=$(intermediates)/export_includes\n" +
		"$(export_includes): PRIVATE_EXPORT_C_INCLUDE_DIRS:=$(LOCAL_EXPORT_C_INCLUDE_DIRS)\n")

	// Define the actual rule, with dependencies on this module's output
	sb.WriteString("$(export_includes): $(" + moduleName +
		"_OUTPUTS) $(LOCAL_MODULE_MAKEFILE_DEP)\n")

	sb.WriteString("\t@echo Export includes file: $< -- $@\n" +
		"\t$(hide) mkdir -p $(dir $@) && rm -f $@\n" +
		"ifdef LOCAL_EXPORT_C_INCLUDE_DIRS\n" +
		"\t$(hide) for d in $(PRIVATE_EXPORT_C_INCLUDE_DIRS); do \\\n" +
		"\t\techo \"-I $$d\" >> $@; \\\n" +
		"\t\tdone\n" +
		"else\n" +
		"\t$(hide) touch $@\n" +
		"endif\n\n" +

		"$(LOCAL_BUILT_MODULE): $(LOCAL_SRC_FILES) | $(export_includes)\n" +
		"\tmkdir -p $(dir $@)\n" +
		"\tcp $< $@\n\n")
}

// The following makefile snippets are based on Android makefiles from AOSP
//  See aosp/build/core/make/prebuilt_internal.mk

// Setup link type
// We assume LOCAL_SDK_VERSION and LOCAL_USE_VNDK will not be set
const libraryLinkTypeMkText string = "ifeq ($(PLATFORM_SDK_VERSION),25)\n" +
	"  # link_type not required\n" +

	// Android O only.
	"else ifeq ($(PLATFORM_SDK_VERSION),26)\n" +
	"  my_link_type := $(intermediates)/link_type\n\n" +

	"$(my_link_type): PRIVATE_LINK_TYPE := native:platform\n" +
	"$(eval $(call link-type-partitions,$(my_link_type)))\n" +
	"$(my_link_type):\n" +
	"\t@echo Check module type: $@\n" +
	"\t$(hide) mkdir -p $(dir $@) && rm -f $@\n" +
	"\t$(hide) echo \"$(PRIVATE_LINK_TYPE)\" >$@\n" +

	"$(LOCAL_BUILT_MODULE): | $(my_link_type)\n\n" +

	// Android OMR1 and later
	"else\n" +
	"  include $(BUILD_SYSTEM)/allowed_ndk_types.mk\n\n" +
	"  my_link_type := native:platform\n" +
	"  my_link_deps :=\n" +
	"  my_2nd_arch_prefix := $(LOCAL_2ND_ARCH_VAR_PREFIX)\n" +
	"  my_common :=\n" +
	"  include $(BUILD_SYSTEM)/link_type.mk\n" +
	"endif\n"

func declarePrebuiltStaticLib(sb *strings.Builder, moduleName, path, includePaths string, target bool) {
	sb.WriteString("\ninclude $(CLEAR_VARS)\n")
	sb.WriteString("LOCAL_MODULE:=" + moduleName + "\n")
	sb.WriteString("LOCAL_SRC_FILES:=" + path + "\n")
	if !target {
		sb.WriteString("LOCAL_IS_HOST_MODULE:=true\n")
	}

	// We would like to just have the following line, but it looks like it is NDK only
	// Therefore all the following is needed.
	//sb.WriteString("include $(PREBUILT_STATIC_LIBRARY)\n")

	sb.WriteString("LOCAL_MODULE_CLASS:=STATIC_LIBRARIES\n")
	sb.WriteString("LOCAL_UNINSTALLABLE_MODULE:=true\n")
	sb.WriteString("LOCAL_MODULE_SUFFIX:=.a\n")

	if includePaths != "" {
		sb.WriteString("LOCAL_EXPORT_C_INCLUDE_DIRS:=" + includePaths + "\n")
	}

	sb.WriteString("include $(BUILD_SYSTEM)/base_rules.mk\n\n")

	writeExportIncludeMkText(sb, moduleName)
	sb.WriteString(libraryLinkTypeMkText)
}

func declarePrebuiltSharedLib(sb *strings.Builder, moduleName, path, includePaths string, target bool) {
	sb.WriteString("\ninclude $(CLEAR_VARS)\n")
	sb.WriteString("LOCAL_MODULE:=" + moduleName + "\n")
	sb.WriteString("LOCAL_SRC_FILES:=" + path + "\n")
	if !target {
		sb.WriteString("LOCAL_IS_HOST_MODULE:=true\n")
	}
	// We would like to just have the following line, but it looks like it is NDK only
	// Therefore all the following is needed.
	//sb.WriteString("include $(PREBUILT_SHARED_LIBRARY)\n")

	sb.WriteString("LOCAL_MODULE_CLASS:=SHARED_LIBRARIES\n")
	sb.WriteString("LOCAL_MODULE_SUFFIX:=.so\n")

	if includePaths != "" {
		sb.WriteString("LOCAL_EXPORT_C_INCLUDE_DIRS:=" + includePaths + "\n")
	}

	//  Put shared libraries in common path to simplify link line.
	//  see shared_library_internal.mk and host_shared_library_internal.mk
	if target {
		// Only supporting the primary target architecture.
		// Others are TARGET_2ND_x
		sb.WriteString("OVERRIDE_BUILT_MODULE_PATH:=$(TARGET_OUT_INTERMEDIATE_LIBRARIES)\n\n")
	} else {
		// Only supporting the primary host architecture.
		// Others are HOST_2ND_x, HOST_CROSS_x, HOST_CROSS_2ND_x
		sb.WriteString("OVERRIDE_BUILT_MODULE_PATH:=$(HOST_OUT_INTERMEDIATE_LIBRARIES)\n\n")
	}

	sb.WriteString("include $(BUILD_SYSTEM)/base_rules.mk\n\n")

	writeExportIncludeMkText(sb, moduleName)
	sb.WriteString(libraryLinkTypeMkText)
}

func declarePrebuiltBinary(sb *strings.Builder, moduleName, path string, target bool) {
	sb.WriteString("\ninclude $(CLEAR_VARS)\n")
	sb.WriteString("LOCAL_MODULE:=" + moduleName + "\n")
	sb.WriteString("LOCAL_SRC_FILES:=" + path + "\n")
	if !target {
		sb.WriteString("LOCAL_IS_HOST_MODULE:=true\n")
	}

	sb.WriteString("LOCAL_MODULE_CLASS:=EXECUTABLES\n")
	sb.WriteString("LOCAL_MODULE_SUFFIX:=\n\n")

	sb.WriteString("include $(BUILD_SYSTEM)/base_rules.mk\n\n")

	sb.WriteString("$(LOCAL_BUILT_MODULE): $(LOCAL_SRC_FILES)\n")
	sb.WriteString("\tmkdir -p $(dir $@)\n")
	sb.WriteString("\tcp $< $@\n")
}

func installGeneratedFiles(sb *strings.Builder, m installable, ctx blueprint.ModuleContext, tags []string) {
	/* Install generated files one by one, if required */
	installBase, installRel, ok := getAndroidInstallPath(m.getInstallableProps())

	if !ok {
		return
	}
	sb.WriteString("\n")
	filesToInstall := m.filesToInstall(ctx)

	for _, file := range filesToInstall {
		moduleName := pathToModuleName(file)

		sb.WriteString("include $(CLEAR_VARS)\n\n")

		sb.WriteString("LOCAL_MODULE := " + moduleName + "\n")
		sb.WriteString("LOCAL_INSTALLED_MODULE_STEM := " + filepath.Base(file) + "\n")
		sb.WriteString("LOCAL_MODULE_CLASS := ETC\n")
		sb.WriteString("LOCAL_MODULE_PATH := " + installBase + "\n")
		sb.WriteString("LOCAL_MODULE_RELATIVE_PATH := " + installRel + "\n")
		writeListAssignment(sb, "LOCAL_MODULE_TAGS", tags)
		sb.WriteString("LOCAL_PREBUILT_MODULE_FILE := " + file + "\n\n")

		sb.WriteString("include $(BUILD_PREBUILT)\n")
	}
}

func (g *androidMkGenerator) generateCommonActions(sb *strings.Builder, m *generateCommon, ctx blueprint.ModuleContext, inouts []inout) {
	m.outputdir = g.sourceOutputDir(m)
	prefixInoutsWithOutputDir(inouts, m.outputDir())
	// Calculate and record outputs and include dirs
	m.recordOutputsFromInout(inouts)
	m.includeDirs = utils.PrefixDirs(m.Properties.Export_gen_include_dirs, m.outputDir())

	sb.WriteString("##########################\ninclude $(CLEAR_VARS)\n\n")

	// This is required to have $(local-generated-sources-dir) work as expected
	sb.WriteString("LOCAL_MODULE := " + m.Name() + "\n")
	sb.WriteString("LOCAL_MODULE_CLASS := STATIC_LIBRARIES\n")
	sb.WriteString(outputsVarName(m) + " := \n")
	sb.WriteString(outputDirVarName(m) + " := " + g.sourceOutputDir(m) + "\n")
	sb.WriteString("\n")

	cmd, args, implicits, _ := m.getArgs(ctx)
	utils.StripUnusedArgs(args, cmd)

	for _, inout := range inouts {
		ins := strings.Join(inout.in, " ")

		// Make does not cleanly support multiple out-files
		// To handle that, we output the rule only on the first file, and let every other output
		// depend on the first.
		// This is not 100 % safe, since if the secondary file is removed, it will not be rebuilt.
		// It is assumed that this will not be a big issue, since removing individual files from a generated
		// directory should not be common.
		outs := inout.out[0]
		for _, key := range utils.SortedKeys(args) {
			sb.WriteString(outs + ": " + key + ":= " + args[key] + "\n")
		}

		sb.WriteString(outs + ": in := " + ins + "\n")
		sb.WriteString(outs + ": out := " + strings.Join(inout.out, " ") + "\n")
		if inout.depfile != "" {
			sb.WriteString(outs + ": depfile := " + inout.depfile + "\n")
		}
		if m.Properties.Rsp_content != nil {
			sb.WriteString(outs + ": rspfile := " + inout.rspfile + "\n")
		}
		if strings.Contains(cmd, "$(LOCAL_PATH)") {
			sb.WriteString(outs + ": LOCAL_PATH := $(LOCAL_PATH)" + "\n")
		}
		sb.WriteString(outs + ": " + ins + " " + strings.Join(inout.implicitSrcs, " ") + "\n")
		// Kati will automatically put entire command lines into RSP files, so we can
		// just echo the content on the command line, even if it exceeds the command
		// line limit. The proper way to do this would be Make's $(file) function,
		// except that it isn't permitted on Kati.
		if m.Properties.Rsp_content != nil {
			sb.WriteString("\techo \"" + *m.Properties.Rsp_content + "\" > \"$(rspfile)\"\n")
		}
		sb.WriteString("\t" + cmd + "\n")
		if inout.depfile != "" {
			// Convert the depfile file format as part of this rule.
			sb.WriteString(g.transformDepFile("$(depfile)"))
			// ...and include it outside the rule.
			sb.WriteString(g.includeDepFile(outs, inout.depfile))
		}
		sb.WriteString(outputsVarName(m) + " += " + outs + "\n")

		for _, out := range append(inout.out[1:], inout.implicitOuts...) {
			sb.WriteString(out + ": " + outs + "\n")
			sb.WriteString(outputsVarName(m) + " += " + out + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString(g.outputs(m) + ": " + strings.Join(implicits, " ") + "\n")

	/* This will ensure that any dependencies will not be rebuilt in the case of no change */
	sb.WriteString(".KATI_RESTAT: " + g.outputs(m) + "\n")
}

func (g *androidMkGenerator) generateSourceActions(m *generateSource, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		inouts := m.generateInouts(ctx, g)
		g.generateCommonActions(sb, &m.generateCommon, ctx, inouts)
		installGeneratedFiles(sb, m, ctx, m.generateCommon.Properties.Tags)
		androidMkWriteString(ctx, m.altShortName(), sb)
	}
}

func (g *androidMkGenerator) transformSourceActions(m *transformSource, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		inouts := m.generateInouts(ctx, g)
		g.generateCommonActions(sb, &m.generateCommon, ctx, inouts)
		installGeneratedFiles(sb, m, ctx, m.generateCommon.Properties.Tags)
		androidMkWriteString(ctx, m.altShortName(), sb)
	}
}

func (g *androidMkGenerator) genStaticActions(m *generateStaticLibrary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		inouts := m.generateInouts(ctx, g)
		g.generateCommonActions(sb, &m.generateCommon, ctx, inouts)

		// Add prebuilt outputs
		outputs := m.outputs()
		if len(outputs) != 1 {
			utils.Die("outputs() returned %d objects for bob_generate_static_lib %s", len(outputs), m.Name())
		}
		library := outputs[0]
		declarePrebuiltStaticLib(sb, m.altShortName(), library,
			strings.Join(m.genIncludeDirs(), " "),
			m.generateCommon.Properties.Target != tgtTypeHost)

		androidMkWriteString(ctx, m.altShortName(), sb)
	}
}

func (g *androidMkGenerator) genSharedActions(m *generateSharedLibrary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		inouts := m.generateInouts(ctx, g)
		g.generateCommonActions(sb, &m.generateCommon, ctx, inouts)

		// Add prebuilt outputs
		outputs := m.outputs()
		if len(outputs) != 1 {
			utils.Die("outputs() returned %d objects for bob_generate_shared_lib %s", len(outputs), m.Name())
		}
		library := outputs[0]
		declarePrebuiltSharedLib(sb, m.altShortName(), library,
			strings.Join(m.genIncludeDirs(), " "),
			m.generateCommon.Properties.Target != tgtTypeHost)

		androidMkWriteString(ctx, m.altShortName(), sb)
	}
}

func (g *androidMkGenerator) genBinaryActions(m *generateBinary, ctx blueprint.ModuleContext) {
	if enabledAndRequired(m) {
		sb := &strings.Builder{}
		inouts := m.generateInouts(ctx, g)
		g.generateCommonActions(sb, &m.generateCommon, ctx, inouts)

		// Add prebuilt outputs
		outputs := m.outputs()
		if len(outputs) != 1 {
			utils.Die("outputs() returned %d objects for bob_generate_binary %s", len(outputs), m.Name())
		}
		binary := outputs[0]
		declarePrebuiltBinary(sb, m.altShortName(), binary,
			m.generateCommon.Properties.Target != tgtTypeHost)

		androidMkWriteString(ctx, m.altShortName(), sb)
	}
}

func (g *androidMkGenerator) binaryOutputDir(m *binary) string {
	return "$(HOST_OUT_EXECUTABLES)"
}

type androidMkOrderer struct {
}

type androidMkFile struct {
	Name string
	Deps []string
}

type androidMkFileSlice []androidMkFile

type androidNaming interface {
	// Alternate name for module output, also used in preference to the
	// module name for the Android module name
	altName() string
	// Alternate brief name for the module
	altShortName() string
}

func generatesAndroidIncFile(m blueprint.Module) bool {
	if _, ok := m.(*defaults); ok {
		return false
	} else if _, ok := m.(*externalLib); ok {
		return false
	}
	return true
}

func (s *androidMkOrderer) GenerateBuildActions(ctx blueprint.SingletonContext) {
	sb := &strings.Builder{}
	var order androidMkFileSlice
	ctx.VisitAllModules(func(m blueprint.Module) {
		di, ok := m.(androidNaming)
		if ok && enabledAndRequired(m) {
			deps := []string{}
			ctx.VisitDepsDepthFirst(m, func(child blueprint.Module) {
				childdi, ok := child.(androidNaming)
				if ok && generatesAndroidIncFile(child) && enabledAndRequired(m) {
					deps = append(deps, childdi.altShortName())
				}
			})
			if generatesAndroidIncFile(m) {
				order = append(order, androidMkFile{di.altShortName(), deps})
			}
		}
	})

	// Quick and dirty alphabetical topological sort
	for len(order) > 0 {
		lowindex := -1
		for i, val := range order {
			if (lowindex == -1 || val.Name < order[lowindex].Name) && len(val.Deps) == 0 {
				lowindex = i
			}
		}
		if lowindex == -1 {

			/* Generate a list of remaining modules and their dependencies */
			deps := ""
			for _, o := range order {
				deps += fmt.Sprintf("%s depends on\n", o.Name)
				for _, d := range o.Deps {
					deps += fmt.Sprintf("\t%s\n", d)
				}
			}

			utils.Die("unmet or circular dependency. %d remaining.\n%s", len(order), deps)
		}

		sb.WriteString("include $(BOB_ANDROIDMK_DIR)/" + order[lowindex].Name + ".inc\n")

		for i := range order {
			newdeps := []string{}
			for _, dep := range order[i].Deps {
				if dep != order[lowindex].Name {
					newdeps = append(newdeps, dep)
				}
			}
			order[i].Deps = newdeps
		}
		order = append(order[:lowindex], order[lowindex+1:]...)
	}
	androidmkFile := getPathInBuildDir("Android.inc")
	err := fileutils.WriteIfChanged(androidmkFile, sb)
	if err != nil {
		utils.Die("%v", err.Error())
	}

	// Blueprint does not output package context dependencies unless
	// the package context outputs a variable, pool or rule to the
	// build.ninja.
	//
	// The Android make backend does not create variables, pools or
	// rules since the build logic is actually written in makefiles.
	// Therefore write a dummy ninja target to ensure that the bob
	// package context dependencies are output.
	//
	// We make the target optional, so that it doesn't execute when
	// ninja runs without a target.
	ctx.Build(pctx,
		blueprint.BuildParams{
			Rule:     dummyRule,
			Outputs:  []string{androidmkFile},
			Optional: true,
		})
}

func (g *androidMkGenerator) moduleOutputDir(moduleName string) string {
	return fmt.Sprintf("$(dir $(ALL_MODULES.%s.BUILT))", moduleName)
}

func (g *androidMkGenerator) kernelModOutputDir(m *kernelModule) string {
	return g.moduleOutputDir(m.altName())
}

func (g *androidMkGenerator) staticLibOutputDir(m *staticLibrary) string {
	return g.moduleOutputDir(m.altName())
}

func (g *androidMkGenerator) sharedLibOutputDir(m *sharedLibrary) string {
	return g.moduleOutputDir(m.altName())
}

func (g *androidMkGenerator) sharedLibsDir(tgt tgtType) string {
	if tgt != tgtTypeHost {
		return "$(TARGET_OUT_SHARED_LIBRARIES)"
	}
	return "$(HOST_OUT_SHARED_LIBRARIES)"
}

func (g *androidMkGenerator) transformDepFile(depfile string) (text string) {
	text += "ifeq ($(word 1, $(subst ., ,$(PLATFORM_VERSION))),7)\n"
	text += "\t$(call transform-d-to-p-args," + depfile + "," + depfile + ".P)\n"
	text += "endif\n"
	return
}

func (g *androidMkGenerator) includeDepFile(target string, depfile string) (text string) {
	text += "ifeq ($(word 1, $(subst ., ,$(PLATFORM_VERSION))),7)\n"
	text += "  $(call include-depfile," + depfile + ".P," + target + ")\n"
	text += "else\n"
	text += "  $(call include-depfile," + depfile + "," + target + ")\n"
	text += "endif\n"
	return
}

const prebuiltMake = "prebuilts/build-tools/linux-x86/bin/make"

var makeCommandArgs = getMakeCommandArgs()

func getMakeCommandArgs() (args []string) {
	if utils.IsExecutable(prebuiltMake) {
		args = []string{"--make-command", prebuiltMake}
	}
	return
}

func (g *androidMkGenerator) kernelModuleActions(m *kernelModule, ctx blueprint.ModuleContext) {
	if !enabledAndRequired(m) {
		return
	}
	// Calculate and record outputs
	m.outputdir = g.kernelModOutputDir(m)
	m.outs = []string{filepath.Join(m.outputDir(), m.outputName()+".ko")}

	sb := &strings.Builder{}
	sb.WriteString("##########################\ninclude $(CLEAR_VARS)\n\n")
	sb.WriteString("LOCAL_MODULE := " + m.altShortName() + "\n")
	sb.WriteString("LOCAL_MODULE_CLASS := KERNEL_MODULES\n")
	sb.WriteString("LOCAL_CLANG := false\n")
	writeListAssignment(sb, "LOCAL_MODULE_TAGS", m.Properties.Tags)
	sb.WriteString("\n")

	sources := m.Properties.getSources(ctx)

	sb.WriteString("LOCAL_SRC_FILES :=" + newlineSeparatedList(sources))

	// Now the build rules (i.e. what would be done by an 'include
	// $(BUILD_KERNEL_MODULE)', if there was one).
	sb.WriteString("TARGET_OUT_$(LOCAL_MODULE_CLASS) := $(TARGET_OUT)/lib/modules\n")
	installBase, installRel, ok := getAndroidInstallPath(&m.Properties.InstallableProps)
	if ok {
		sb.WriteString("LOCAL_MODULE_PATH := " + installBase + "\n")
		sb.WriteString("LOCAL_MODULE_RELATIVE_PATH := " + installRel + "\n")
	} else {
		sb.WriteString("LOCAL_UNINSTALLABLE_MODULE := true\n")
	}
	sb.WriteString("LOCAL_MODULE_SUFFIX := .ko\n")
	if m.Properties.isProprietary() {
		sb.WriteString("LOCAL_MODULE_OWNER := " + proptools.String(m.Properties.Owner) + "\n")
		sb.WriteString("LOCAL_PROPRIETARY_MODULE := true\n")
	}
	sb.WriteString("include $(BUILD_SYSTEM)/base_rules.mk\n\n")

	args := m.generateKbuildArgs(ctx).toDict()
	args["sources"] = utils.Join([]string{"$(addprefix $(LOCAL_PATH)/,$(LOCAL_SRC_FILES))"},
		m.extraSymbolsFiles(ctx))
	args["local_path"] = "$(LOCAL_PATH)"
	args["make_command_args"] = strings.Join(makeCommandArgs, " ")

	// Create a target-specific variable declaration for each required parameter.
	for _, key := range utils.SortedKeys(args) {
		sb.WriteString(fmt.Sprintf("$(LOCAL_BUILT_MODULE): %s := %s\n", key, args[key]))
	}

	sb.WriteString("\n$(LOCAL_BUILT_MODULE): $(LOCAL_MODULE_MAKEFILE_DEP) " +
		args["sources"] + " " + args["kmod_build"] + "\n")
	sb.WriteString("\tmkdir -p \"$(@D)\"\n")
	cmd := "python $(kmod_build) --output $@ --depfile $@.d $(make_command_args) " +
		"--common-root $(local_path) " +
		"--module-dir \"$(output_module_dir)\" $(extra_includes) " +
		"--sources $(sources) " +
		"--kernel \"$(kernel_dir)\" --cross-compile \"$(kernel_cross_compile)\" " +
		"$(cc_flag) $(hostcc_flag) $(clang_triple_flag) $(ld_flag) " +
		"$(kbuild_options) --extra-cflags=\"$(extra_cflags)\" $(make_args)"

	sb.WriteString("\techo " + cmd + "\n")
	sb.WriteString("\t" + cmd + "\n")
	sb.WriteString(g.transformDepFile("$@.d") + "\n")

	sb.WriteString(g.includeDepFile("$(LOCAL_BUILT_MODULE)", "$(LOCAL_BUILT_MODULE).d"))

	// The Module.symvers file is generated during Kbuild, but make doesn't
	// support multiple output files in a rule, so add it as a dependency
	// of the module.
	sb.WriteString(fmt.Sprintf("\n$(dir $(LOCAL_BUILT_MODULE))/Module.symvers: $(LOCAL_BUILT_MODULE)\n"))

	androidMkWriteString(ctx, m.altShortName(), sb)
}

func androidMkOrdererFactory() blueprint.Singleton {
	return &androidMkOrderer{}
}

var (
	androidModuleNameMap    = map[string]string{}
	androidModuleReverseMap = map[string]string{}
	androidModuleMapLock    sync.RWMutex
)

func androidModuleName(name string) string {
	androidModuleMapLock.RLock()
	defer androidModuleMapLock.RUnlock()
	return androidModuleNameMap[name]
}

func androidModuleNames(names []string) (androidNames []string) {
	for _, name := range names {
		androidNames = append(androidNames, androidModuleName(name))
	}
	return
}

func mapAndroidNames(ctx blueprint.BottomUpMutatorContext) {
	if m, ok := ctx.Module().(androidNaming); ok {
		// Ignore defaults
		if _, ok := ctx.Module().(*defaults); ok {
			return
		}

		if enabledAndRequired(ctx.Module()) {
			androidModuleMapLock.Lock()
			defer androidModuleMapLock.Unlock()

			if existing, ok := androidModuleReverseMap[m.altName()]; ok {
				if existing != ctx.ModuleName() {
					utils.Die("out name collision. Both %s and %s are required and map to %s",
						ctx.ModuleName(), existing, m.altName())
				}
			}
			androidModuleNameMap[ctx.ModuleName()] = m.altName()
			androidModuleReverseMap[m.altName()] = ctx.ModuleName()
		}
	}
}

func (g *androidMkGenerator) init(ctx *blueprint.Context, config *bobConfig) {
	ctx.RegisterBottomUpMutator("modulemapper", mapAndroidNames).Parallel()

	ctx.RegisterSingletonType("androidmk_orderer", androidMkOrdererFactory)

	g.toolchainSet.parseConfig(config)
}
