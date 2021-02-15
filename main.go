package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage/driver"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
)

var settings = cli.New()

func main() {
	var (
		suppressSecretPatch bool
		disableValidation   bool
	)
	valueOpts := &values.Options{}
	changes := false
	var rootCmd = &cobra.Command{
		Use:   "helmdiff <NAME> <CHART>",
		Short: "Preview helm upgrade changes as a JSON patch",
		Long:  "Preview helm upgrade changes as a JSON patch",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validateReleaseName(name); err != nil {
				log.Fatal(err)
			}

			chartPath := args[1]

			vals, err := valueOpts.MergeValues(getter.All(settings))
			if err != nil {
				log.Fatal(err)
			}

			ch, err := loader.Load(chartPath)
			if err != nil {
				log.Fatal(err)
			}

			patchset, err := createPatchset(name, ch, vals, suppressSecretPatch, disableValidation)
			if err != nil {
				log.Fatal(err)
			}

			dec := json.NewDecoder(strings.NewReader(patchset))
			var patchlist []interface{}
			if err := dec.Decode(&patchlist); err != nil {
				log.Fatal(err)
			}

			if len(patchlist) > 0 {
				changes = true
			} else {
				return nil
			}

			outputFlag := cmd.Flag("output")
			switch outputFlag.Value.String() {
			case "json":
				output, err := json.MarshalIndent(&patchlist, "", "  ")
				if err != nil {
					log.Fatalf("error: %v", err)
				}
				fmt.Printf("%s\n", string(output))
			case "yaml":
				output, err := yaml.Marshal(&patchlist)
				if err != nil {
					log.Fatalf("error: %v", err)
				}
				fmt.Printf("%s\n", string(output))
			case "quiet":
				if changes == true {
					os.Exit(2)
				}
			default:
				log.Fatalf("Output 'type' %s is not supported", outputFlag.Value.String())
			}

			if changes == true {
				fmt.Printf("Changes in release %s detected\n", name)
				os.Exit(2)
			}

			return nil
		},
	}

	f := rootCmd.Flags()
	f.StringP("output", "o", "yaml", fmt.Sprintf("prints the output in the specified format. Allowed values: json, yaml, quiet"))
	f.BoolVar(&suppressSecretPatch, "suppress-secrets-patch", false, "suppress secrets in the output")
	f.BoolVar(&disableValidation, "disable-openapi-validation", false, "Don't validate against OpenAPI schema")
	settings.AddFlags(f)
	addValueOptionsFlags(f, valueOpts)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func createPatchset(name string, ch *chart.Chart, vals map[string]interface{}, suppressSecretPatch bool, disableValidation bool) (string, error) {
	patches := []string{}
	patchstring := ""

	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		log.Fatalf("%+v", err)
	}

	if err := actionConfig.KubeClient.IsReachable(); err != nil {
		return "", err
	}

	originalManifest, targetManifest, err := prepareUpgrade(actionConfig, name, ch, vals)
	if err != nil {
		return "", err
	}

	original, err := actionConfig.KubeClient.Build(bytes.NewBufferString(originalManifest), !disableValidation)
	if err != nil {
		return "", errors.Wrap(err, "unable to build kubernetes objects from original release manifest")
	}
	target, err := actionConfig.KubeClient.Build(bytes.NewBufferString(targetManifest), !disableValidation)
	if err != nil {
		return "", errors.Wrap(err, "unable to build kubernetes objects from new release manifest")
	}

	deleted := original.Difference(target)
	for _, objInfo := range deleted {
		patchstring = fmt.Sprintf("{\"action\": \"delete\", \"object\": \"%s\", \"namespace\": \"%s\", \"patch\": {}}", objInfo.ObjectName(), objInfo.Namespace)
		patches = append(patches, string(patchstring))
	}

	err = target.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		showSecrets := true
		if info.Mapping.Resource.Resource == "secrets" && suppressSecretPatch {
			showSecrets = false
		}

		helper := resource.NewHelper(info.Client, info.Mapping)
		if _, err := helper.Get(info.Namespace, info.Name, info.Export); apierrors.IsNotFound(err) {
			//handle new objects
			manifest, err := json.Marshal(info.Object)
			if err != nil {
				return errors.Wrap(err, "unable to marshal new added kubernetes resources")
			}
			if showSecrets {
				patchstring = fmt.Sprintf("{\"action\": \"create\", \"object\": \"%s\", \"namespace\": \"%s\", \"patch\": %s}", info.ObjectName(), info.Namespace, manifest)
			} else {
				patchstring = fmt.Sprintf("{\"action\": \"create\", \"object\": \"%s\", \"namespace\": \"%s\", \"patch\": \"SECRET PATCH WAS SUPPRESSED\"}", info.ObjectName(), info.Namespace)
			}
			patches = append(patches, string(patchstring))
			return nil
		}

		originalInfo := original.Get(info)
		if originalInfo == nil {
			return fmt.Errorf("could not find %q", info.Name)
		}

		patch, _, err := createPatch(originalInfo.Object, info)
		if err != nil {
			return err
		}

		if len(patch) > 2 {
			if showSecrets {
				patchstring = fmt.Sprintf("{\"action\": \"patch\", \"object\": \"%s\", \"namespace\": \"%s\", \"patch\": %s}", info.ObjectName(), info.Namespace, patch)
			} else {
				patchstring = fmt.Sprintf("{\"action\": \"patch\", \"object\": \"%s\", \"namespace\": \"%s\", \"patch\": \"SECRET PATCH WAS SUPPRESSED\"}", info.ObjectName(), info.Namespace)
			}
			patches = append(patches, string(patchstring))
		}
		return nil
	})

	return fmt.Sprintf("[%s]", strings.Join(patches, ",")), err
}

func prepareUpgrade(c *action.Configuration, name string, chart *chart.Chart, vals map[string]interface{}) (string, string, error) {
	if chart == nil {
		return "", "", errors.New("missing chart")
	}

	// finds the last non-deleted release with the given name
	lastRelease, err := c.Releases.Last(name)
	if err != nil {
		// to keep existing behavior of returning the "%q has no deployed releases" error when an existing release does not exist
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return "", "", driver.NewErrNoDeployedReleases(name)
		}
		return "", "", err
	}

	var currentRelease *release.Release
	if lastRelease.Info.Status == release.StatusDeployed {
		// no need to retrieve the last deployed release from storage as the last release is deployed
		currentRelease = lastRelease
	} else {
		// finds the deployed release with the given name
		currentRelease, err = c.Releases.Deployed(name)
		if err != nil {
			if errors.Is(err, driver.ErrNoDeployedReleases) &&
				(lastRelease.Info.Status == release.StatusFailed || lastRelease.Info.Status == release.StatusSuperseded) {
				currentRelease = lastRelease
			} else {
				return "", "", err
			}
		}
	}

	if err := chartutil.ProcessDependencies(chart, vals); err != nil {
		return "", "", err
	}

	// Increment revision count. This is passed to templates, and also stored on
	// the release object.
	revision := lastRelease.Version + 1

	options := chartutil.ReleaseOptions{
		Name:      name,
		Namespace: currentRelease.Namespace,
		Revision:  revision,
		IsUpgrade: true,
	}

	if err := getCapabilities(c); err != nil {
		return "", "", err
	}
	valuesToRender, err := chartutil.ToRenderValues(chart, vals, options, c.Capabilities)
	if err != nil {
		return "", "", err
	}

	manifestDoc, err := renderResources(c, chart, valuesToRender)
	if err != nil {
		return "", "", err
	}

	return currentRelease.Manifest, manifestDoc.String(), err
}

// capabilities builds a Capabilities from discovery information.
func getCapabilities(c *action.Configuration) error {
	if c.Capabilities != nil {
		return nil
	}
	dc, err := c.RESTClientGetter.ToDiscoveryClient()
	if err != nil {
		return errors.Wrap(err, "could not get Kubernetes discovery client")
	}
	// force a discovery cache invalidation to always fetch the latest server version/capabilities.
	dc.Invalidate()
	kubeVersion, err := dc.ServerVersion()
	if err != nil {
		return errors.Wrap(err, "could not get server version from Kubernetes")
	}
	// Issue #6361:
	// Client-Go emits an error when an API service is registered but unimplemented.
	// We trap that error here and print a warning. But since the discovery client continues
	// building the API object, it is correctly populated with all valid APIs.
	// See https://github.com/kubernetes/kubernetes/issues/72051#issuecomment-521157642
	apiVersions, err := action.GetVersionSet(dc)
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			c.Log("WARNING: The Kubernetes server has an orphaned API service. Server reports: %s", err)
			c.Log("WARNING: To fix this, kubectl delete apiservice <service-name>")
		} else {
			return errors.Wrap(err, "could not get apiVersions from Kubernetes")
		}
	}

	c.Capabilities = &chartutil.Capabilities{
		APIVersions: apiVersions,
		KubeVersion: chartutil.KubeVersion{
			Version: kubeVersion.GitVersion,
			Major:   kubeVersion.Major,
			Minor:   kubeVersion.Minor,
		},
	}
	return nil
}

func renderResources(c *action.Configuration, ch *chart.Chart, values chartutil.Values) (*bytes.Buffer, error) {
	b := bytes.NewBuffer(nil)

	if err := getCapabilities(c); err != nil {
		return b, err
	}

	if ch.Metadata.KubeVersion != "" {
		if !chartutil.IsCompatibleRange(ch.Metadata.KubeVersion, c.Capabilities.KubeVersion.String()) {
			return b, errors.Errorf("chart requires kubeVersion: %s which is incompatible with Kubernetes %s", ch.Metadata.KubeVersion, c.Capabilities.KubeVersion.String())
		}
	}

	rest, err := c.RESTClientGetter.ToRESTConfig()
	if err != nil {
		return b, err
	}
	files, err := engine.RenderWithClient(ch, values, rest)
	if err != nil {
		return b, err
	}

	// Sort hooks, manifests, and partials. Only hooks and manifests are returned,
	// as partials are not used after renderer.Render. Empty manifests are also
	// removed here.
	_, manifests, err := releaseutil.SortManifests(files, c.Capabilities.APIVersions, releaseutil.InstallOrder)
	if err != nil {
		return b, err
	}

	for _, m := range manifests {
		// skip notes
		if !strings.Contains(m.Name, "NOTES.txt") {
			fmt.Fprintf(b, "---\n# Source: %s\n%s\n", m.Name, m.Content)
		}
	}

	return b, nil
}

func createPatch(current runtime.Object, target *resource.Info) ([]byte, types.PatchType, error) {
	oldData, err := json.Marshal(current)
	if err != nil {
		return nil, types.StrategicMergePatchType, errors.Wrap(err, "serializing current configuration")
	}
	newData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, types.StrategicMergePatchType, errors.Wrap(err, "serializing target configuration")
	}

	// Fetch the current object for the three way merge
	helper := resource.NewHelper(target.Client, target.Mapping)
	currentObj, err := helper.Get(target.Namespace, target.Name, target.Export)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, types.StrategicMergePatchType, errors.Wrapf(err, "unable to get data for current object %s/%s", target.Namespace, target.Name)
	}

	// Even if currentObj is nil (because it was not found), it will marshal just fine
	currentData, err := json.Marshal(currentObj)
	if err != nil {
		return nil, types.StrategicMergePatchType, errors.Wrap(err, "serializing live configuration")
	}

	// Get a versioned object
	versionedObject := kube.AsVersioned(target)

	// Unstructured objects, such as CRDs, may not have an not registered error
	// returned from ConvertToVersion. Anything that's unstructured should
	// use the jsonpatch.CreateMergePatch. Strategic Merge Patch is not supported
	// on objects like CRDs.
	_, isUnstructured := versionedObject.(runtime.Unstructured)

	// On newer K8s versions, CRDs aren't unstructured but has this dedicated type
	_, isCRD := versionedObject.(*apiextv1.CustomResourceDefinition)

	if isUnstructured || isCRD {
		// fall back to generic JSON merge patch
		patch, err := jsonpatch.CreateMergePatch(oldData, newData)
		return patch, types.MergePatchType, err
	}

	patchMeta, err := strategicpatch.NewPatchMetaFromStruct(versionedObject)
	if err != nil {
		return nil, types.StrategicMergePatchType, errors.Wrap(err, "unable to create patch metadata from object")
	}

	patch, err := strategicpatch.CreateThreeWayMergePatch(oldData, newData, currentData, patchMeta, true)
	return patch, types.StrategicMergePatchType, err
}

func validateReleaseName(releaseName string) error {
	if releaseName == "" {
		return fmt.Errorf("no release name set")
	}

	// Check length first, since that is a less expensive operation.
	if len(releaseName) > 53 || !action.ValidName.MatchString(releaseName) {
		return fmt.Errorf("invalid release name: %s", releaseName)
	}

	return nil
}

func addValueOptionsFlags(f *pflag.FlagSet, v *values.Options) {
	f.StringSliceVarP(&v.ValueFiles, "values", "f", []string{}, "specify values in a YAML file or a URL (can specify multiple)")
	f.StringArrayVar(&v.Values, "set", []string{}, "set values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.StringArrayVar(&v.StringValues, "set-string", []string{}, "set STRING values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.StringArrayVar(&v.FileValues, "set-file", []string{}, "set values from respective files specified via the command line (can specify multiple or separate values with commas: key1=path1,key2=path2)")
}
