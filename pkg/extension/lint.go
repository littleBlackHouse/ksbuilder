package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/lint/support"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/yaml"

	"github.com/kubesphere/ksbuilder/cmd/options"
	"github.com/kubesphere/ksbuilder/pkg/helm"
)

func WithHelm(o *options.LintOptions, paths []string) error {
	fmt.Print("\n#################### lint by helm ####################\n")
	if o.Client.WithSubcharts {
		for _, p := range paths {
			if err := filepath.Walk(filepath.Join(p, "charts"), func(path string, info os.FileInfo, err error) error {
				if info != nil {
					if info.Name() == "Chart.yaml" {
						paths = append(paths, filepath.Dir(path))
					} else if strings.HasSuffix(path, ".tgz") || strings.HasSuffix(path, ".tar.gz") {
						paths = append(paths, path)
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}

	o.Client.Namespace = o.Settings.Namespace()
	vals, err := o.ValueOpts.MergeValues(getter.All(o.Settings))
	if err != nil {
		return err
	}

	var message strings.Builder
	failed := 0
	errorsOrWarnings := 0

	for _, path := range paths {
		metadata, err := LoadMetadata(paths[0])
		if err != nil {
			return err
		}
		chartYaml, err := metadata.ToChartYaml()
		if err != nil {
			return err
		}

		result := helm.Lint(o.Client, []string{path}, vals, chartYaml)

		// If there is no errors/warnings and quiet flag is set
		// go to the next chart
		hasWarningsOrErrors := action.HasWarningsOrErrors(result)
		if hasWarningsOrErrors {
			errorsOrWarnings++
		}
		if o.Client.Quiet && !hasWarningsOrErrors {
			continue
		}

		fmt.Fprintf(&message, "==> Linting %s\n", path)

		// All the Errors that are generated by a chart
		// that failed a lint will be included in the
		// results.Messages so we only need to print
		// the Errors if there are no Messages.
		if len(result.Messages) == 0 {
			for _, err := range result.Errors {
				fmt.Fprintf(&message, "Error %s\n", err)
			}
		}

		for _, msg := range result.Messages {
			if !o.Client.Quiet || msg.Severity > support.InfoSev {
				fmt.Fprintf(&message, "%s\n", msg)
			}
		}

		if len(result.Errors) != 0 {
			failed++
		}

		// Adding extra new line here to break up the
		// results, stops this from being a big wall of
		// text and makes it easier to follow.
		fmt.Fprint(&message, "\n")
	}

	fmt.Print(message.String())

	summary := fmt.Sprintf("%d chart(s) linted, %d chart(s) failed", len(paths), failed)
	if failed > 0 {
		return fmt.Errorf(summary)
	}
	if !o.Client.Quiet || errorsOrWarnings > 0 {
		fmt.Print(summary)
	}
	return nil
}

func WithBuiltins(paths []string) error {
	fmt.Print("\n#################### lint by kubesphere ####################\n")
	ext, err := Load(paths[0])
	if err != nil {
		return err
	}
	chartYaml, err := ext.Metadata.ToChartYaml()
	if err != nil {
		return err
	}
	chartRequested, err := helm.Load(paths[0], chartYaml)
	if err != nil {
		return err
	}

	if err := lintExtensionsImages(*chartRequested, paths[0], ext.Metadata.Images); err != nil {
		return err
	}
	if err := lintGlobalImageRegistry(*chartRequested, paths[0]); err != nil {
		return err
	}
	if err := lintGlobalNodeSelector(*chartRequested, paths[0]); err != nil {
		return err
	}
	return nil
}

func lintExtensionsImages(charts chart.Chart, extension string, images []string) error {
	fmt.Print("\nInfo: lint images\n")
	if len(images) == 0 {
		fmt.Printf("WARNING: extension %s has no images\n", extension)
		return nil
	}

	files, err := getTemplateFile(&charts, &values.Options{})
	if err != nil {
		return err
	}

	for _, image := range images {
		for name, content := range files {
			// only find in yaml files
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			if strings.Contains(content, image) {
				goto found
			}
		}
		fmt.Printf("ERROR: image %s has not found\n", image)
	found:
	}
	return nil
}

func lintGlobalNodeSelector(charts chart.Chart, extension string) error {
	fmt.Print("\nInfo: lint global.nodeSelector\n")
	key := rand.String(12)
	files, err := getTemplateFile(&charts, &values.Options{
		JSONValues: []string{fmt.Sprintf("global.nodeSelector={\"kubernetes.io/os\": \"%s\"}", key)},
	})
	if err != nil {
		return err
	}

	for name, content := range files {
		// only find in yaml files
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		yamlArr := strings.Split(content, "---")
		for _, y := range yamlArr {
			yamlMap := make(map[string]any)
			if err := yaml.Unmarshal([]byte(y), &yamlMap); err != nil {
				return err
			}
			switch yamlMap["kind"] {
			case "Deployment", "StatefulSet", "ReplicaSet", "Job":
				if yamlMap["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] == nil ||
					yamlMap["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"].(map[string]any)["kubernetes.io/os"] != key {
					fmt.Printf("ERROR: golobal.nodeSelector doesn't work in extension: %s file: %s Resource: {kind %s, name:%s }\n", extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
				}

			case "Pod":
				if yamlMap["spec"].(map[string]any)["nodeSelector"] == nil ||
					yamlMap["spec"].(map[string]any)["nodeSelector"].(map[string]any)["kubernetes.io/os"] != key {
					fmt.Printf("ERROR: golobal.nodeSelector doesn't work in extension: %s file: %s Resource: {kind %s, name:%s }\n", extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
				}
			case "CronJob":
				if yamlMap["spec"].(map[string]any)["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] == nil ||
					yamlMap["spec"].(map[string]any)["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"].(map[string]any)["kubernetes.io/os"] != key {
					fmt.Printf("ERROR: golobal.nodeSelector doesn't work in extension: %s file: %s Resource: {kind %s, name:%s }\n", extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
				}
			}
		}
	}
	return nil
}

func lintGlobalImageRegistry(charts chart.Chart, extension string) error {
	fmt.Print("\nInfo: lint global.imageRegistry\n")
	key := rand.String(12)
	files, err := getTemplateFile(&charts, &values.Options{
		Values: []string{fmt.Sprintf("global.imageRegistry=%s", key)},
	})
	if err != nil {
		return err
	}

	for name, content := range files {
		// only find in yaml files
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		yamlArr := strings.Split(content, "---")
		for _, y := range yamlArr {
			yamlMap := make(map[string]any)
			if err := yaml.Unmarshal([]byte(y), &yamlMap); err != nil {
				return err
			}
			switch yamlMap["kind"] {
			case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
				// init container
				if initContainer, ok := yamlMap["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["initContainers"].([]any); ok {
					for _, c := range initContainer {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in init-cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}
				// container
				if container, ok := yamlMap["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any); ok {
					for _, c := range container {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}

			case "Pod":
				// init container
				if initContainer, ok := yamlMap["spec"].(map[string]any)["initContainers"].([]any); ok {
					for _, c := range initContainer {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in init-cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}
				// container
				if container, ok := yamlMap["spec"].(map[string]any)["containers"].([]any); ok {
					for _, c := range container {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}

			case "CronJob":
				// init container
				if initContainer, ok := yamlMap["spec"].(map[string]any)["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["initContainers"].([]any); ok {
					for _, c := range initContainer {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in init-cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}
				// container
				if container, ok := yamlMap["spec"].(map[string]any)["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any); ok {
					for _, c := range container {
						if !strings.Contains(c.(map[string]any)["image"].(string), key) {
							fmt.Printf("ERROR: golobal.imageRegistry doesn't work in cotainer %s of extension: %s file: %s Resource: {kind %s, name:%s }\n", c.(map[string]any)["name"], extension, name, yamlMap["kind"], yamlMap["metadata"].(map[string]any)["name"])
						}
					}
				}
			}
		}
	}
	return nil
}

func getTemplateFile(chartRequested *chart.Chart, valueOpts *values.Options) (map[string]string, error) {
	p := getter.All(cli.New())
	vals, err := valueOpts.MergeValues(p)
	if err != nil {
		return nil, err
	}

	if err := chartutil.ProcessDependenciesWithMerge(chartRequested, vals); err != nil {
		return nil, err
	}

	topVals, err := chartutil.CoalesceValues(chartRequested, vals)
	if err != nil {
		return nil, err
	}
	top := map[string]interface{}{
		"Chart":        chartRequested.Metadata,
		"Capabilities": chartutil.DefaultCapabilities.Copy(),
		// set Release undefined
		"Release": map[string]interface{}{
			"Name":      "undefined",
			"Namespace": "undefined",
			"Revision":  1,
			"Service":   "Helm",
		},
		"Values": topVals,
	}

	return engine.Render(chartRequested, top)
}
