package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/birmacher/steps-register-ios-device/device"
	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-steputils/tools"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-steplib/steps-deploy-to-itunesconnect-deliver/appleauth"
	"github.com/bitrise-steplib/steps-deploy-to-itunesconnect-deliver/devportalservice"
	"github.com/bitrise-steplib/steps-ios-auto-provision-appstoreconnect/appstoreconnect"
	"github.com/bitrise-steplib/steps-ios-auto-provision-appstoreconnect/autoprovision"
)

const noDeveloperAccountConnectedWarning = `Connected Apple Developer Portal Account not found.
Most likely because there is no Apple Developer Portal Account connected to the build.
Read more: https://devcenter.bitrise.io/getting-started/configuring-bitrise-steps-that-require-apple-developer-account-data/`

func handleSessionDataError(err error) {
	if err == nil {
		return
	}

	if networkErr, ok := err.(devportalservice.NetworkError); ok && networkErr.Status == http.StatusUnauthorized {
		log.Warnf("Building a Pull Request for a Public App. Secret environments are not available in this build to protect them.\nThis will prevent us to fetch Bitrise Apple Developer Portal connection.")
		return
	}

	log.Errorf("Failed to activate Bitrise Apple Developer Portal connection:\n%v", err)
	log.Warnf("Falling back to step inputs.\nRead more about this issue: https://devcenter.bitrise.io/getting-started/configuring-bitrise-steps-that-require-apple-developer-account-data/")
}

func setupStepConfigs() (Config, error) {
	var stepConf Config
	if err := stepconf.Parse(&stepConf); err != nil {
		return Config{}, fmt.Errorf("Failed to read step configs:\n%s", err)
	}
	stepconf.Print(stepConf)
	log.Printf("")

	return stepConf, nil
}

func setupAppStoreConnectAPIClient(config Config) (*appstoreconnect.Client, error) {
	// Creating AppstoreConnectAPI client
	log.Infof("Setup App Store Connect API connection")

	// Setup API connections
	authInputs := appleauth.Inputs{
		APIIssuer:  config.APIIssuer,
		APIKeyPath: string(config.APIKeyPath),
	}
	if err := authInputs.Validate(); err != nil {
		return nil, fmt.Errorf("Failed to validate App Store Connect API inputs:\n%v", err)
	}

	// Authentication sources
	// First try to authenticate with connected account fetched from Bitrise
	// if it fails try with configs fetched from step configs
	authSources := []appleauth.Source{
		&appleauth.ConnectionAPIKeySource{},
		&appleauth.InputAPIKeySource{},
	}

	// Setup connection with the connected account stored on bitrise.io
	var devportalConnectionProvider *devportalservice.BitriseClient
	var appleDeveloperPortalConnection *devportalservice.AppleDeveloperConnection
	if config.BuildURL != "" && config.BuildAPIToken != "" {
		devportalConnectionProvider = devportalservice.NewBitriseClient(http.DefaultClient, config.BuildURL, string(config.BuildAPIToken))

		if devportalConnectionProvider != nil {
			var err error
			appleDeveloperPortalConnection, err = devportalConnectionProvider.GetAppleDeveloperConnection()
			if err != nil {
				handleSessionDataError(err)
			}
		}

		if appleDeveloperPortalConnection == nil || (appleDeveloperPortalConnection.APIKeyConnection == nil) {
			log.Warnf("%s", noDeveloperAccountConnectedWarning)
		}
	} else {
		log.Warnf("Failed to fetch connected Apple Developer Portal Account from bitrise.io.\nStep is not running on bitrise.io: BITRISE_BUILD_URL and BITRISE_BUILD_API_TOKEN envs are not set")
	}

	// Setup configs with newly acquired bitrise account, or fall back to step inputs
	authConfig, err := appleauth.Select(appleDeveloperPortalConnection, authSources, authInputs)
	if err != nil {
		return nil, fmt.Errorf("Failed to configure App Store Connect API authentication:\n%v", err)
	}

	// Setup connection
	client := appstoreconnect.NewClient(http.DefaultClient, authConfig.APIKey.KeyID, authConfig.APIKey.IssuerID, []byte(authConfig.APIKey.PrivateKey))
	client.EnableDebugLogs = false

	log.Donef("Successfully setup connection to Apple Developer Portal")
	return client, nil
}

func logErrorAndExitIfAny(err error) {
	if err != nil {
		log.Errorf("%v", err)
		os.Exit(1)
	}
}

func main() {
	config, err := setupStepConfigs()
	logErrorAndExitIfAny(err)

	client, err := setupAppStoreConnectAPIClient(config)
	logErrorAndExitIfAny(err)

	err = device.RegisterDevices(client, []device.Device{
		{
			Name:     config.DeviceName,
			UDID:     config.DeviceUDID,
			Platform: config.DevicePlatform,
		},
	})
	logErrorAndExitIfAny(err)

	// This will need to be moved out from this step
	// for the experiment I'll leave it here as it's easier this way

	// find all provisioning profiles
	var profilePaths = []string{}
	err = filepath.Walk(config.XcarchivePath,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if strings.HasSuffix(path, "embedded.mobileprovision") {
				profilePaths = append(profilePaths, path)
			}
			return nil
		})
	if err != nil {
		logErrorAndExitIfAny(fmt.Errorf("Failed to read Xcarchive file: %s\n%v", config.XcarchivePath, err))
	}

	profileNames := make(map[string]string)
	var distributionType appstoreconnect.ProfileType = ""

	xcarchiveInfoPlist := path.Join(config.XcarchivePath, "Info.plist")
	teamID, err := GetValueForKeyInPlist(xcarchiveInfoPlist, ":ApplicationProperties:Team")
	if err != nil {
		logErrorAndExitIfAny(fmt.Errorf("Failed to read Xcarchive's Info.plist file at path: %s\n%v", xcarchiveInfoPlist, err))
	}

	signingIdentity, err := GetValueForKeyInPlist(xcarchiveInfoPlist, ":ApplicationProperties:SigningIdentity")
	if err != nil {
		logErrorAndExitIfAny(fmt.Errorf("Failed to read Xcarchive's Info.plist file at path: %s\n%v", xcarchiveInfoPlist, err))
	}

	// list profiles
	for _, profilePath := range profilePaths {
		log.Printf("")
		log.Infof("Provisioning profile located at: %s", profilePath)

		// locate Info.plist
		infoPlistPath := path.Join(path.Dir(profilePath), "Info.plist")
		bundleIdentifier, err := GetValueForKeyInPlist(infoPlistPath, ":CFBundleIdentifier")
		if err != nil {
			logErrorAndExitIfAny(fmt.Errorf("Failed to read Info.plist file at path: %s\n%v", infoPlistPath, err))
		}

		// get name
		name, err := GetValueForKeyInProvisioningProfile(profilePath, "Name")
		if err != nil {
			logErrorAndExitIfAny(fmt.Errorf("%s", name))
		}
		profileNames[bundleIdentifier] = name

		// get platform
		platform, err := GetValueForKeyInProvisioningProfile(profilePath, "Platform")
		if err != nil {
			logErrorAndExitIfAny(fmt.Errorf("%s", platform))
		}

		if !strings.Contains(platform, "iOS") {
			log.Warnf("Provisioning profile platform is not iOS. Skipping...")
			continue
		}

		// get device UUIDs
		_, err = GetValueForKeyInProvisioningProfile(profilePath, ":ProvisionedDevices")
		if err != nil {
			logErrorAndExitIfAny(fmt.Errorf("Cannot resign with provisioning profile type: AppStore, or Enterprise provisioning profile detected."))
		}

		// get distribution type for the file to export
		if bundleIdentifier == config.BundleIDToExport {
			distributionBoolenFlag, err := GetValueForKeyInProvisioningProfile(profilePath, ":Entitlements:get-task-allow")
			if err != nil {
				logErrorAndExitIfAny(fmt.Errorf("%s", distributionBoolenFlag))
			}

			if distributionBoolenFlag == "true" {
				distributionType = appstoreconnect.IOSAppDevelopment
			} else {
				distributionType = appstoreconnect.IOSAppAdHoc
			}
		}

		profile, err := FindProfileWithName(client, name)
		logErrorAndExitIfAny(err)

		devicesInProfile, err := GetDevices(client, profile)
		logErrorAndExitIfAny(err)

		deviceFound := false
		for _, deviceInProfile := range devicesInProfile {
			if deviceInProfile.Attributes.UDID == config.DeviceUDID {
				log.Warnf("Device already added to this provisioning profile. Skipping...")

				deviceFound = true
				break
			}
		}
		if deviceFound == true {
			continue
		}

		log.Printf("Attempting to update provisioning profile on Apple Developer Portal: %s", profile.Attributes.Name)

		// BundleID
		bundleID, err := GetBundleID(client, profile)
		logErrorAndExitIfAny(err)

		// Certificates
		certificateIDs, err := GetCertificates(client, profile)
		logErrorAndExitIfAny(err)

		// Devices
		deviceIDs, err := GetAllRegisteredDevices(client, profile)
		logErrorAndExitIfAny(err)

		// Delete profile
		log.Printf("Deleting original provisioning profile on Apple Developer Portal")
		err = autoprovision.DeleteProfile(client, profile.ID)
		logErrorAndExitIfAny(err)

		// Create profile
		log.Printf("Recreating provisioning profile on Apple Developer Portal")
		profile, err = autoprovision.CreateProfile(
			client,
			profile.Attributes.Name,
			profile.Attributes.ProfileType,
			*bundleID,
			certificateIDs,
			deviceIDs,
		)
		logErrorAndExitIfAny(err)

		log.Donef("Provisioning profile %s (%s) successfully created on Apple Deveper Portal", profile.Attributes.Name, profile.Attributes.UUID)
	}

	log.Printf("")
	log.Infof("Installing provisioning profiles")
	for _, profileName := range profileNames {
		profile, err := FindProfileWithName(client, profileName)
		logErrorAndExitIfAny(err)

		err = DownloadProvisioningProfile(client, *profile)
		logErrorAndExitIfAny(err)
	}
	log.Donef("Successfully installed provisioning profiles")

	log.Printf("")
	xcarchiveExportOptions := XCarxhiveExportOption(config.BundleIDToExport, distributionType.ReadableString(), signingIdentity, profileNames, teamID)
	if err := tools.ExportEnvironmentWithEnvman("BITRISE_XCARCHIVE_EXPORT_OPTIONS", xcarchiveExportOptions); err != nil {
		logErrorAndExitIfAny(fmt.Errorf("Failed to export BITRISE_XCARCHIVE_EXPORT_OPTIONS\n%v", err))
	}
	log.Donef("Xcarchive export options exported to BITRISE_XCARCHIVE_EXPORT_OPTIONS environment variable")

	os.Exit(0)
}

func GetBundleID(client *appstoreconnect.Client, profile *appstoreconnect.Profile) (*appstoreconnect.BundleID, error) {
	bundleIDResponse, err := client.Provisioning.BundleID(profile.Relationships.BundleID.Links.Related)
	logErrorAndExitIfAny(err)

	return autoprovision.FindBundleID(client, bundleIDResponse.Data.Attributes.Identifier)
}

func GetDevices(client *appstoreconnect.Client, profile *appstoreconnect.Profile) ([]appstoreconnect.Device, error) {
	var devices []appstoreconnect.Device
	var nextPageURL string

	for {
		response, err := client.Provisioning.Devices(
			profile.Relationships.Devices.Links.Related,
			&appstoreconnect.PagingOptions{
				Limit: 20,
				Next:  nextPageURL,
			},
		)
		if err != nil {
			return []appstoreconnect.Device{}, err
		}

		devices = append(devices, response.Data...)

		nextPageURL = response.Links.Next
		if nextPageURL == "" {
			break
		}
	}

	return devices, nil
}

func GetCertificates(client *appstoreconnect.Client, profile *appstoreconnect.Profile) ([]string, error) {
	var certificateIDs []string
	var nextPageURL string

	for {
		response, err := client.Provisioning.Certificates(
			profile.Relationships.Certificates.Links.Related,
			&appstoreconnect.PagingOptions{
				Limit: 20,
				Next:  nextPageURL,
			},
		)
		if err != nil {
			return []string{}, err
		}

		var certificates []appstoreconnect.Certificate = response.Data
		for _, certificate := range certificates {
			certificateIDs = append(certificateIDs, certificate.ID)
		}

		nextPageURL = response.Links.Next
		if nextPageURL == "" {
			break
		}
	}

	return certificateIDs, nil
}

func GetAllRegisteredDevices(client *appstoreconnect.Client, profile *appstoreconnect.Profile) ([]string, error) {
	var deviceIDs []string

	devices, err := autoprovision.ListDevices(client, "", appstoreconnect.IOSDevice)
	if err != nil {
		return []string{}, err
	}

	for _, device := range devices {
		if strings.HasPrefix(string(profile.Attributes.ProfileType), "TVOS") && device.Attributes.DeviceClass != "APPLE_TV" {
			continue
		} else if strings.HasPrefix(string(profile.Attributes.ProfileType), "IOS") &&
			string(device.Attributes.DeviceClass) != "IPHONE" && string(device.Attributes.DeviceClass) != "IPAD" && string(device.Attributes.DeviceClass) != "IPOD" && string(device.Attributes.DeviceClass) != "APPLE_WATCH" {
			continue
		}
		deviceIDs = append(deviceIDs, device.ID)
	}

	return deviceIDs, nil
}

func GetValueForKeyInProvisioningProfile(filePath string, key string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("security", "cms", "-D", "-i", filePath)
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var tmpFilePath string = os.TempDir() + "resign-profile.plist"
	os.Remove(tmpFilePath)

	ioutil.WriteFile(filepath.Join(tmpFilePath), out.Bytes(), 0644)
	defer func() {
		os.Remove(tmpFilePath)
	}()

	return GetValueForKeyInPlist(tmpFilePath, key)
}

func GetValueForKeyInPlist(filePath string, key string) (string, error) {
	cmd := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print "+key, filePath)
	bytes, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(bytes)), err
}

func DownloadProvisioningProfile(client *appstoreconnect.Client, profile appstoreconnect.Profile) error {
	log.Printf("Installing provisioning profile: %s (%s)", profile.Attributes.Name, profile.Attributes.UUID)

	err := autoprovision.WriteProfile(profile)
	if err != nil {
		return fmt.Errorf("Failed to install profile %s (%s)\n%v", profile.Attributes.Name, profile.Attributes.UUID, err)
	}
	return nil
}

func FindProfileWithName(client *appstoreconnect.Client, profileName string) (*appstoreconnect.Profile, error) {
	// find profiles with name
	profiles, err := FindProfile(client, profileName)
	if err != nil {
		return nil, err
	}

	var profile *appstoreconnect.Profile = nil
	for _, p := range profiles {
		if p.Attributes.Name != profileName {
			continue
		}

		profile = &p
		break
	}
	if profile == nil {
		return nil, fmt.Errorf("Failed to locate Provisioning Profile on Apple Developer Portal with name: %s", profileName)
	}

	return profile, nil
}

// ListProfiles ...
func FindProfile(client *appstoreconnect.Client, name string) ([]appstoreconnect.Profile, error) {
	opt := &appstoreconnect.ListProfilesOptions{
		PagingOptions: appstoreconnect.PagingOptions{
			Limit: 100,
		},
		FilterName: name,
	}

	r, err := client.Provisioning.ListProfiles(opt)
	if err != nil {
		return nil, err
	}

	if len(r.Data) == 0 {
		return nil, nil
	}

	return r.Data, nil
}

//
func XCarxhiveExportOption(exportBundleID string, distributionType string, certificate string, bundleIDsWithProfiles map[string]string, teamID string) string {
	provisioningProfiles := ""
	for bundleID, profileName := range bundleIDsWithProfiles {
		profileIdentifier := fmt.Sprintf(`
			<key>%s</key>
			<string>%s</string>`,
			bundleID,
			profileName)

		if len(provisioningProfiles) == 0 {
			provisioningProfiles = profileIdentifier
		} else {
			profileIdentifier = strings.Join([]string{
				provisioningProfiles,
				profileIdentifier,
			}, "\n")
		}
	}

	return fmt.Sprintf(`
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
	<dict>
		<key>distributionBundleIdentifier</key>
		<string>%s</string>
		<key>method</key>
		<string>%s</string>
		<key>provisioningProfiles</key>
		<dict>%s
		</dict>
		<key>signingCertificate</key>
		<string>%s</string>
		<key>teamID</key>
		<string>%s</string>
	</dict>
</plist>
`,
		exportBundleID,
		distributionType,
		provisioningProfiles,
		certificate,
		teamID,
	)
}
