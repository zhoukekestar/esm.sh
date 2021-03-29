package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/ije/gox/crypto/rs"
	"github.com/ije/gox/utils"
	"github.com/postui/postdb"
	"github.com/postui/postdb/q"
)

const (
	jsCopyrightName = "esm.sh"
)

var (
	buildVersion = 1
)

var targets = map[string]api.Target{
	"deno":   api.ESNext,
	"es2015": api.ES2015,
	"es2016": api.ES2016,
	"es2017": api.ES2017,
	"es2018": api.ES2018,
	"es2019": api.ES2019,
	"es2020": api.ES2020,
}

// todo: use queue to replace lock
var buildLock sync.Mutex

// ImportMeta defines import meta
type ImportMeta struct {
	*NpmPackage
	Exports []string `json:"exports"`
	Dts     string   `json:"dts"`
}

type buildOptions struct {
	packages moduleSlice
	external moduleSlice
	target   string
	isDev    bool
}

type buildResult struct {
	buildID    string
	importMeta map[string]*ImportMeta
	hasCSS     bool
}

func build(storageDir string, hostname string, options buildOptions) (ret buildResult, err error) {
	n := len(options.packages)
	if n == 0 {
		err = fmt.Errorf("no packages")
		return
	}

	single := n == 1
	if single {
		pkg := options.packages[0]
		filename := path.Base(pkg.name)
		target := options.target
		if len(options.external) > 0 {
			target = fmt.Sprintf("external=%s/%s", strings.ReplaceAll(options.external.String(), "/", "_"), target)
		}
		if pkg.submodule != "" {
			filename = pkg.submodule
		}
		if options.isDev {
			filename += ".development"
		}
		ret.buildID = fmt.Sprintf("v%d/%s@%s/%s/%s", buildVersion, pkg.name, pkg.version, target, filename)
	} else {
		hash := sha1.New()
		sort.Sort(options.packages)
		sort.Sort(options.external)
		fmt.Fprintf(hash, "v%d/%s/%s/%s/%v", buildVersion, options.packages.String(), options.external.String(), options.target, options.isDev)
		ret.buildID = "bundle-" + strings.ToLower(base32.StdEncoding.EncodeToString(hash.Sum(nil)))
	}

	p, err := db.Get(q.Alias(ret.buildID), q.K("importMeta", "css"))
	if err == nil {
		err = json.Unmarshal(p.KV.Get("importMeta"), &ret.importMeta)
		if err != nil {
			_, err = db.Delete(q.Alias(ret.buildID))
			if err != nil {
				return
			}
		}

		if val := p.KV.Get("css"); len(val) == 1 && val[0] == 1 {
			ret.hasCSS = fileExists(path.Join(storageDir, "builds", ret.buildID+".css"))
		}

		if fileExists(path.Join(storageDir, "builds", ret.buildID+".js")) {
			// has built
			return
		}

		_, err = db.Delete(q.Alias(ret.buildID))
		if err != nil {
			return
		}
	}
	if err != nil && err != postdb.ErrNotFound {
		return
	}

	buildLock.Lock()
	defer buildLock.Unlock()

	// todo: add stand-alone cjs-module-lexer service
	installList := []string{}
	for _, pkg := range options.packages {
		installList = append(installList, pkg.name+"@"+pkg.version)
	}

	start := time.Now()
	importMeta := map[string]*ImportMeta{}
	peerDependencies := map[string]string{}
	for _, pkg := range options.packages {
		var p NpmPackage
		p, err = nodeEnv.getPackageInfo(pkg.name, pkg.version)
		if err != nil {
			return
		}
		meta := &ImportMeta{
			NpmPackage: &p,
		}
		for name, version := range p.PeerDependencies {
			if name == "react" && p.Name == "react-dom" {
				version = p.Version
			}
			peerDependencies[name] = version
		}
		if meta.Types == "" && meta.Typings == "" && !strings.HasPrefix(pkg.name, "@") {
			var info NpmPackage
			info, err = nodeEnv.getPackageInfo("@types/"+pkg.name, "latest")
			if err == nil {
				if info.Types != "" || info.Typings != "" || info.Main != "" {
					installList = append(installList, fmt.Sprintf("%s@%s", info.Name, info.Version))
				}
			} else if err.Error() != fmt.Sprintf("npm: package '@types/%s' not found", pkg.name) {
				return
			}
		}
		if meta.Module == "" && meta.Type == "module" {
			meta.Module = meta.Main
		}
		if meta.Module == "" && meta.DefinedExports["import"] != "" {
			meta.Module = meta.DefinedExports["import"]
		}
		if pkg.submodule != "" {
			meta.Main = pkg.submodule
			meta.Module = ""
			meta.Types = ""
			meta.Typings = ""
		}
		importMeta[pkg.ImportPath()] = meta
	}

	peerPackages := map[string]NpmPackage{}
	for name, version := range peerDependencies {
		peer := true
		for _, pkg := range options.packages {
			if pkg.name == name {
				peer = false
				break
			}
		}
		if peer {
			for _, meta := range importMeta {
				for dep := range meta.Dependencies {
					if dep == name {
						peer = false
						break
					}
				}
			}
		}
		if peer {
			peerPackages[name] = NpmPackage{
				Name: name,
			}
			for _, m := range options.external {
				if m.name == name {
					version = m.version
					break
				}
			}
			installList = append(installList, name+"@"+version)
		}
	}

	log.Debugf("parse importMeta in %v", time.Now().Sub(start))

	buildDir := path.Join(os.TempDir(), "esmd-build", rs.Hex.String(16))
	nodeModulesDir := path.Join(buildDir, "node_modules")
	ensureDir(buildDir)
	defer os.RemoveAll(buildDir)

	err = os.Chdir(buildDir)
	if err != nil {
		return
	}

	err = yarnAdd(installList...)
	if err != nil {
		return
	}

	env := "production"
	if options.isDev {
		env = "development"
	}

	for _, pkg := range options.packages {
		importPath := pkg.ImportPath()
		meta := importMeta[importPath]
		pkgDir := path.Join(nodeModulesDir, meta.Name)

		if pkg.submodule != "" {
			if fileExists(path.Join(pkgDir, pkg.submodule, "package.json")) {
				var p NpmPackage
				err = utils.ParseJSONFile(path.Join(pkgDir, pkg.submodule, "package.json"), &p)
				if err != nil {
					return
				}
				if p.Main != "" {
					meta.Main = path.Join(pkg.submodule, p.Main)
				}
				if p.Module != "" {
					meta.Module = path.Join(pkg.submodule, p.Module)
				} else if meta.Type == "module" && p.Main != "" {
					meta.Module = path.Join(pkg.submodule, p.Main)
				}
				if p.Types != "" {
					meta.Types = path.Join(pkg.submodule, p.Types)
				}
				if p.Typings != "" {
					meta.Typings = path.Join(pkg.submodule, p.Typings)
				}
			} else {
				exports, esm, e := parseESModuleExports(buildDir, path.Join(meta.Name, pkg.submodule))
				if e != nil {
					err = e
					return
				}
				if esm {
					meta.Module = pkg.submodule
					meta.Exports = exports
					continue
				}
			}
		}

		if meta.Module != "" {
			exports, esm, e := parseESModuleExports(buildDir, path.Join(meta.Name, meta.Module))
			if e != nil {
				err = e
				return
			}
			if esm {
				meta.Exports = exports
				continue
			}

			// fake module
			meta.Module = ""
		}

		meta.Exports, err = parseCJSModuleExports(buildDir, importPath)
		if err != nil {
			return
		}
	}

	start = time.Now()
	hasTypes := false
	for _, pkg := range options.packages {
		var types string
		meta := importMeta[pkg.ImportPath()]
		nv := fmt.Sprintf("%s@%s", meta.Name, meta.Version)
		if meta.Types != "" || meta.Typings != "" {
			types = getTypesPath(nodeModulesDir, *meta.NpmPackage, "")
		} else if pkg.submodule == "" {
			if fileExists(path.Join(nodeModulesDir, pkg.name, "index.d.ts")) {
				types = fmt.Sprintf("%s/%s", nv, "index.d.ts")
			} else if !strings.HasPrefix(pkg.name, "@") {
				var info NpmPackage
				err = utils.ParseJSONFile(path.Join(nodeModulesDir, "@types", pkg.name, "package.json"), &info)
				if err == nil {
					types = getTypesPath(nodeModulesDir, info, "")
				} else if !os.IsNotExist(err) {
					return
				}
			}
		} else {
			if fileExists(path.Join(nodeModulesDir, pkg.name, pkg.submodule, "index.d.ts")) {
				types = fmt.Sprintf("%s/%s", nv, path.Join(pkg.submodule, "index.d.ts"))
			} else if fileExists(path.Join(nodeModulesDir, pkg.name, ensureExt(pkg.submodule, ".d.ts"))) {
				types = fmt.Sprintf("%s/%s", nv, ensureExt(pkg.submodule, ".d.ts"))
			} else if fileExists(path.Join(nodeModulesDir, "@types", pkg.name, pkg.submodule, "index.d.ts")) {
				types = fmt.Sprintf("@types/%s/%s", nv, path.Join(pkg.submodule, "index.d.ts"))
			} else if fileExists(path.Join(nodeModulesDir, "@types", pkg.name, ensureExt(pkg.submodule, ".d.ts"))) {
				types = fmt.Sprintf("@types/%s/%s", nv, ensureExt(pkg.submodule, ".d.ts"))
			}
		}
		if types != "" {
			err = copyDTS(options.external, hostname, nodeModulesDir, path.Join(storageDir, "types", fmt.Sprintf("v%d", buildVersion)), types)
			if err != nil {
				err = fmt.Errorf("copyDTS(%s): %v", types, err)
				return
			}
			meta.Dts = "/" + types
			hasTypes = true
		}
	}
	if hasTypes {
		log.Debug("copy dts in", time.Now().Sub(start))
	}

	externals := make([]string, len(peerPackages)+len(builtInNodeModules)+len(options.external))
	i := 0
	for name := range peerPackages {
		var p NpmPackage
		err = utils.ParseJSONFile(path.Join(nodeModulesDir, name, "package.json"), &p)
		if err != nil {
			return
		}
		peerPackages[name] = p
		externals[i] = name
		i++
	}
	for name := range builtInNodeModules {
		var self bool
		for _, pkg := range options.packages {
			if pkg.name == name {
				self = true
			}
		}
		if !self {
			externals[i] = name
			i++
		}
	}
	for _, m := range options.external {
		var self bool
		for _, pkg := range options.packages {
			if pkg.name == m.name {
				self = true
			}
		}
		if !self {
			externals[i] = m.name
			i++
		}
	}
	externals = externals[:i]

	buf := bytes.NewBuffer(nil)
	if single {
		pkg := options.packages[0]
		importPath := pkg.ImportPath()
		importIdentifier := "__" + identify(importPath)
		meta := importMeta[importPath]
		exports := []string{}
		hasDefaultExport := false
		for _, name := range meta.Exports {
			if name == "default" {
				hasDefaultExport = true
			} else if name != "import" {
				exports = append(exports, name)
			}
		}
		if meta.Module != "" {
			if len(exports) > 0 {
				fmt.Fprintf(buf, `export * from "%s";%s`, importPath, EOL)
			}
			if hasDefaultExport {
				fmt.Fprintf(buf, `export { default } from "%s";`, importPath)
			}
		} else {
			fmt.Fprintf(buf, `import %s_default from "%s";%s`, importIdentifier, importPath, EOL)
			if len(exports) > 0 {
				fmt.Fprintf(buf, `import * as %s_star from "%s";%s`, importIdentifier, importPath, EOL)
				fmt.Fprintf(buf, `export const { %s } = %s_star;%s`, strings.Join(exports, ","), importIdentifier, EOL)
			}
			fmt.Fprintf(buf, `export default %s_default;`, importIdentifier)
		}
	} else {
		for _, pkg := range options.packages {
			importPath := pkg.ImportPath()
			importIdentifier := identify(importPath)
			meta := importMeta[importPath]
			hasDefaultExport := false
			for _, name := range meta.Exports {
				if name == "default" {
					hasDefaultExport = true
					break
				}
			}
			if meta.Module != "" {
				fmt.Fprintf(buf, `export * as %s_star from "%s";%s`, importIdentifier, importPath, EOL)
				if hasDefaultExport {
					fmt.Fprintf(buf, `export {default as %s_default} from "%s";`, importIdentifier, importPath)
				}
			} else if meta.Main != "" {
				if hasDefaultExport {
					fmt.Fprintf(buf, `import %s from "%s";%s`, importIdentifier, importPath, EOL)
				} else {
					fmt.Fprintf(buf, `import * as %s from "%s";%s`, importIdentifier, importPath, EOL)
				}
				fmt.Fprintf(buf, `export {%s as %s_default};`, importIdentifier, importIdentifier)
			} else {
				fmt.Fprintf(buf, `export const %s_default = null;`, importIdentifier)
			}
		}
	}
	input := &api.StdinOptions{
		Contents:   buf.String(),
		ResolveDir: buildDir,
		Sourcefile: "export.js",
	}
	minify := !options.isDev
	define := map[string]string{
		"__filename":                  fmt.Sprintf(`"https://%s/%s.js"`, hostname, ret.buildID),
		"__dirname":                   fmt.Sprintf(`"https://%s/%s"`, hostname, path.Dir(ret.buildID)),
		"process":                     "__process$",
		"Buffer":                      "__Buffer$",
		"setImmediate":                "__setImmediate$",
		"clearImmediate":              "clearTimeout",
		"require.resolve":             "__rResolve$",
		"process.env.NODE_ENV":        fmt.Sprintf(`"%s"`, env),
		"global":                      "__global$",
		"global.process":              "__process$",
		"global.Buffer":               "__Buffer$",
		"global.setImmediate":         "__setImmediate$",
		"global.clearImmediate":       "clearTimeout",
		"global.require.resolve":      "__rResolve$",
		"global.process.env.NODE_ENV": fmt.Sprintf(`"%s"`, env),
	}
	indirectRequires := newStringSet()
esbuild:
	start = time.Now()
	peerModulesForCommonjs := newStringMap()
	result := api.Build(api.BuildOptions{
		Stdin:             input,
		Bundle:            true,
		Write:             false,
		Target:            targets[options.target],
		Format:            api.FormatESModule,
		MinifyWhitespace:  minify,
		MinifyIdentifiers: minify,
		MinifySyntax:      minify,
		Define:            define,
		Outdir:            "/esbuild",
		Plugins: []api.Plugin{
			{
				Name: "rewrite-external-path",
				Setup: func(plugin api.PluginBuild) {
					plugin.OnResolve(
						api.OnResolveOptions{Filter: fmt.Sprintf("^(%s)$", strings.Join(externals, "|"))},
						func(args api.OnResolveArgs) (api.OnResolveResult, error) {
							if single {
								pkg := options.packages[0]
								importPath := pkg.ImportPath()
								if args.Path == importPath {
									meta := importMeta[importPath]
									resolvePath := path.Join(nodeModulesDir, meta.Name, ensureExt(meta.Main, ".js"))
									if !fileExists(resolvePath) {
										resolvePath = path.Join(nodeModulesDir, meta.Name, meta.Main, "index.js")
									}
									if fileExists(resolvePath) {
										return api.OnResolveResult{Path: resolvePath}, nil
									}
									return api.OnResolveResult{Path: args.Path, External: true}, nil
								}
							}
							var version string
							var ok bool
							if !ok {
								m, yes := options.external.Get(args.Path)
								if yes {
									version = m.version
									ok = true
								}
							}
							if !ok {
								p, yes := peerPackages[args.Path]
								if yes {
									version = p.Version
									ok = true
								}
							}
							resolvePath := args.Path
							_, esm, _ := parseESModuleExports(buildDir, args.Importer)
							if !ok {
								if options.target == "deno" {
									_, yes := denoStdNodeModules[resolvePath]
									if yes {
										pathname := fmt.Sprintf("/v%d/_deno_std_node_%s.js", buildVersion, resolvePath)
										if esm {
											resolvePath = pathname
										} else {
											peerModulesForCommonjs.Set(resolvePath, pathname)
										}
										return api.OnResolveResult{Path: resolvePath, External: true}, nil
									}
								}

								polyfill, yes := polyfilledBuiltInNodeModules[resolvePath]
								if yes {
									p, err := nodeEnv.getPackageInfo(polyfill, "latest")
									if err == nil {
										resolvePath = polyfill
										version = p.Version
										ok = true
									} else {
										return api.OnResolveResult{Path: resolvePath}, err
									}
								} else {
									_, err := embedFS.Open(fmt.Sprintf("polyfills/node_%s.js", resolvePath))
									if err == nil {
										pathname := fmt.Sprintf("/v%d/_node_%s.js", buildVersion, resolvePath)
										if esm {
											resolvePath = pathname
										} else {
											peerModulesForCommonjs.Set(resolvePath, pathname)
										}
										return api.OnResolveResult{Path: resolvePath, External: true}, nil
									}
								}
							}
							if ok {
								packageName := resolvePath
								if !strings.HasPrefix(packageName, "@") {
									packageName, _ = utils.SplitByFirstByte(packageName, '/')
								}
								filename := path.Base(resolvePath)
								if options.isDev {
									filename += ".development"
								}
								pathname := fmt.Sprintf("/v%d/%s@%s/%s/%s", buildVersion, packageName, version, options.target, ensureExt(filename, ".js"))
								if esm {
									resolvePath = pathname
								} else {
									peerModulesForCommonjs.Set(resolvePath, pathname)
								}
							} else {
								if esm {
									if hostname != "localhost" {
										resolvePath = fmt.Sprintf("https://%s/_error.js?type=resolve&name=%s", hostname, url.QueryEscape(resolvePath))
									} else {
										resolvePath = fmt.Sprintf("/_error.js?type=resolve&name=%s", url.QueryEscape(resolvePath))
									}
								} else {
									peerModulesForCommonjs.Set(resolvePath, "")
								}
							}
							return api.OnResolveResult{Path: resolvePath, External: true}, nil
						},
					)
				},
			},
		},
	})
	for _, w := range result.Warnings {
		if !strings.HasPrefix(w.Text, `Indirect calls to "require" will not be bundled`) {
			log.Warn(w.Text)
		}
	}
	if len(result.Errors) > 0 {
		extraExternals := []string{}
		for _, e := range result.Errors {
			if strings.HasPrefix(e.Text, `Could not resolve "`) {
				missingModule := strings.Split(e.Text, `"`)[1]
				if missingModule != "" {
					if !indirectRequires.Has(missingModule) {
						indirectRequires.Add(missingModule)
						extraExternals = append(extraExternals, missingModule)
					}
				}
			} else {
				err = errors.New("esbuild: " + e.Text)
				return
			}
		}
		if len(extraExternals) > 0 {
			externals = append(externals, extraExternals...)
			goto esbuild // rebuild
		}
	}

	log.Debugf("esbuild %s %s %s in %v", options.packages.String(), options.target, env, time.Now().Sub(start))

	var eol string
	if options.isDev {
		eol = EOL
	}

	jsContentBuf := bytes.NewBuffer(nil)
	fmt.Fprintf(jsContentBuf, `/* %s - esbuild bundle(%s) %s %s */%s`, jsCopyrightName, options.packages.String(), strings.ToLower(options.target), env, EOL)
	if options.isDev {
		deps := map[string]string{}
		for _, pkg := range options.packages {
			importPath := pkg.ImportPath()
			meta := importMeta[importPath]
			if len(meta.Dependencies) > 0 {
				for name, version := range meta.Dependencies {
					deps[name] = version
				}
			}
		}
		if len(deps) > 0 {
			fmt.Fprintf(jsContentBuf, `/*%s * bundled dependencies:%s`, EOL, EOL)
			for name, version := range deps {
				fmt.Fprintf(jsContentBuf, ` *   - %s: %s%s`, name, version, EOL)
			}
			fmt.Fprintf(jsContentBuf, ` */%s`, EOL)
		}
	}

	hasCSS := []byte{0}
	for _, file := range result.OutputFiles {
		outputContent := file.Contents
		if strings.HasSuffix(file.Path, ".js") {
			// add nodejs/deno compatibility
			if bytes.Contains(outputContent, []byte("__process$")) {
				fmt.Fprintf(jsContentBuf, `import __process$ from "/v%d/_node_process.js";%s__process$.env.NODE_ENV="%s";%s`, buildVersion, eol, env, eol)
			}
			if bytes.Contains(outputContent, []byte("__Buffer$")) {
				fmt.Fprintf(jsContentBuf, `import { Buffer as __Buffer$ } from "/v%d/_node_buffer.js";%s`, buildVersion, eol)
			}
			if peerModulesForCommonjs.Size() > 0 {
				for _, entry := range peerModulesForCommonjs.Entries() {
					name, importPath := entry[0], entry[1]
					if importPath != "" {
						identifier := identify(name)
						fmt.Fprintf(jsContentBuf, `import __%s$ from "%s";%s`, identifier, importPath, eol)
						outputContent = bytes.ReplaceAll(
							outputContent,
							[]byte(fmt.Sprintf("require(\"%s\")", name)),
							[]byte(fmt.Sprintf("__%s$", identifier)),
						)
					}
				}
			}

			if bytes.Contains(outputContent, []byte("__global$")) {
				fmt.Fprintf(jsContentBuf, `if (typeof __global$ === "undefined") var __global$ = window;%s`, eol)
			}

			if bytes.Contains(outputContent, []byte("__setImmediate$$")) {
				fmt.Fprintf(jsContentBuf, `__setImmediate$ = (cb, args) => setTimeout(cb, 0, ...args);%s`, eol)
			}

			if bytes.Contains(outputContent, []byte("__rResolve$")) {
				fmt.Fprintf(jsContentBuf, `var __rResolve$ = v => v;%s`, eol)
			}

			// esbuild output
			jsContentBuf.Write(outputContent)
		} else if strings.HasSuffix(file.Path, ".css") {
			saveFilePath := path.Join(storageDir, "builds", ret.buildID+".css")
			ensureDir(path.Dir(saveFilePath))
			file, e := os.Create(saveFilePath)
			if e != nil {
				err = e
				return
			}
			defer file.Close()

			_, err = io.Copy(file, bytes.NewReader(outputContent))
			if err != nil {
				return
			}
			hasCSS = []byte{1}
		}
	}

	saveFilePath := path.Join(storageDir, "builds", ret.buildID+".js")
	ensureDir(path.Dir(saveFilePath))
	file, err := os.Create(saveFilePath)
	if err != nil {
		return
	}
	defer file.Close()

	_, err = io.Copy(file, jsContentBuf)
	if err != nil {
		return
	}

	db.Put(
		q.Alias(ret.buildID),
		q.Tags("build"),
		q.KV{
			"importMeta": utils.MustEncodeJSON(importMeta),
			"css":        hasCSS,
		},
	)

	ret.importMeta = importMeta
	ret.hasCSS = hasCSS[0] == 1
	return
}

func identify(importPath string) string {
	p := []byte(importPath)
	for i, c := range p {
		switch c {
		case '/', '-', '@', '.':
			p[i] = '_'
		default:
			p[i] = c
		}
	}
	return string(p)
}

func getTypesPath(nodeModulesDir string, p NpmPackage, subpath string) string {
	var types string
	if subpath != "" {
		var subpkg NpmPackage
		var subtypes string
		subpkgJSONFile := path.Join(nodeModulesDir, p.Name, subpath, "package.json")
		if fileExists(subpkgJSONFile) && utils.ParseJSONFile(subpkgJSONFile, &subpkg) == nil {
			if subpkg.Types != "" {
				subtypes = subpkg.Types
			} else if subpkg.Typings != "" {
				subtypes = subpkg.Typings
			}
		}
		if subtypes != "" {
			types = path.Join("/", subpath, subtypes)
		} else {
			types = subpath
		}
	} else {
		if p.Types != "" {
			types = p.Types
		} else if p.Typings != "" {
			types = p.Typings
		} else if p.Main != "" {
			types = strings.TrimSuffix(p.Main, ".js")
		} else {
			types = "index.d.ts"
		}
	}
	return fmt.Sprintf("%s@%s%s", p.Name, p.Version, ensureExt(path.Join("/", types), ".d.ts"))
}
