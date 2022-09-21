package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/BishopFox/cloudfox/console"
	"github.com/BishopFox/cloudfox/utils"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/sirupsen/logrus"
)

type SecretsModule struct {
	// General configuration data
	SecretsManagerClient *secretsmanager.Client
	SSMClient            *ssm.Client

	Caller     sts.GetCallerIdentityOutput
	AWSRegions []string
	AWSProfile string

	// Main module data
	Secrets []Secret

	CommandCounter console.CommandCounter
	// Used to store output data for pretty printing
	output utils.OutputData2

	modLog *logrus.Entry
}

type Secret struct {
	AWSService  string
	Region      string
	Name        string
	Description string
}

func (m *SecretsModule) PrintSecrets(outputFormat string, outputDirectory string, verbosity int) {
	// These stuct values are used by the output module
	m.output.Verbosity = verbosity
	m.output.Directory = outputDirectory
	m.output.CallingModule = "secrets"
	m.modLog = utils.TxtLogger.WithFields(logrus.Fields{
		"module": m.output.CallingModule,
	})
	if m.AWSProfile == "" {
		m.AWSProfile = fmt.Sprintf("%s-%s", aws.ToString(m.Caller.Account), aws.ToString(m.Caller.UserId))
	}

	fmt.Printf("[%s] Enumerating secrets for account %s.\n", cyan(m.output.CallingModule), aws.ToString(m.Caller.Account))
	fmt.Printf("[%s] Supported Services: SecretsManager, SSM Parameters\n", cyan(m.output.CallingModule))

	wg := new(sync.WaitGroup)

	// Create a channel to signal the spinner aka task status goroutine to finish
	spinnerDone := make(chan bool)
	//fire up the the task status spinner/updated
	go console.SpinUntil(m.output.CallingModule, &m.CommandCounter, spinnerDone, "regions")

	//create a channel to receive the objects
	dataReceiver := make(chan Secret)

	// Create a channel to signal to stop
	receiverDone := make(chan bool)
	go m.Receiver(dataReceiver, receiverDone)

	for _, region := range m.AWSRegions {
		wg.Add(1)
		m.CommandCounter.Pending++
		go m.executeChecks(region, wg, dataReceiver)
	}

	wg.Wait()
	// Send a message to the spinner goroutine to close the channel and stop
	spinnerDone <- true
	<-spinnerDone
	// Send a message to the data receiver goroutine to close the channel and stop
	receiverDone <- true
	<-receiverDone

	//	fmt.Printf("\nAnalyzed Resources by Region\n\n")

	m.output.Headers = []string{
		"Service",
		"Region",
		"Name",
		"Description",
	}

	// Table rows
	for i := range m.Secrets {
		m.output.Body = append(
			m.output.Body,
			[]string{
				m.Secrets[i].AWSService,
				m.Secrets[i].Region,
				m.Secrets[i].Name,
				m.Secrets[i].Description,
			},
		)

	}
	if len(m.output.Body) > 0 {

		m.output.FilePath = filepath.Join(outputDirectory, "cloudfox-output", "aws", m.AWSProfile)
		//m.output.OutputSelector(outputFormat)
		utils.OutputSelector(m.output.Verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule)
		m.writeLoot(outputDirectory, verbosity)
		fmt.Printf("[%s] %s secrets found.\n", cyan(m.output.CallingModule), strconv.Itoa(len(m.output.Body)))

	} else {
		fmt.Printf("[%s] No secrets found, skipping the creation of an output file.\n", cyan(m.output.CallingModule))
	}

}

func (m *SecretsModule) Receiver(receiver chan Secret, receiverDone chan bool) {
	defer close(receiverDone)
	for {
		select {
		case data := <-receiver:
			m.Secrets = append(m.Secrets, data)
		case <-receiverDone:
			receiverDone <- true
			return
		}
	}
}

func (m *SecretsModule) executeChecks(r string, wg *sync.WaitGroup, dataReceiver chan Secret) {
	defer wg.Done()
	m.CommandCounter.Total++
	m.CommandCounter.Pending--
	m.CommandCounter.Executing++
	m.getSecretsManagerSecretsPerRegion(r, dataReceiver)
	m.getSSMParametersPerRegion(r, dataReceiver)
	m.CommandCounter.Executing--
	m.CommandCounter.Complete++
}

func (m *SecretsModule) writeLoot(outputDirectory string, verbosity int) {
	path := filepath.Join(outputDirectory, "cloudfox-output", "aws", m.AWSProfile, "loot")
	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
	}
	pullFile := filepath.Join(path, "pull-secrets-commands.txt")

	var out string
	out = out + fmt.Sprintln("#############################################")
	out = out + fmt.Sprintln("# The profile you will use to perform these commands is most likely not the profile you used to run CloudFox")
	out = out + fmt.Sprintln("# Set the $profile environment variable to the profile you are going to use to pull the secrets/parameters.")
	out = out + fmt.Sprintln("# E.g., export profile=dev-prod.")
	out = out + fmt.Sprintln("#############################################")
	out = out + fmt.Sprintln("")

	for _, secret := range m.Secrets {
		if secret.AWSService == "SecretsManager" {
			out = out + fmt.Sprintf("aws --profile $profile --region %s secretsmanager get-secret-value --secret-id %s\n", secret.Region, secret.Name)
		}
		if secret.AWSService == "SSM" {
			out = out + fmt.Sprintf("aws --profile $profile --region %s ssm get-parameter --with-decryption --name %s\n", secret.Region, secret.Name)
		}
	}
	err = os.WriteFile(pullFile, []byte(out), 0644)
	if err != nil {
		m.modLog.Error(err.Error())
		m.CommandCounter.Error++
	}

	if verbosity > 2 {
		fmt.Println()
		fmt.Printf("[%s] %s \n\n", cyan(m.output.CallingModule), green("Use the commands below to retrieve the secrets that look interesting"))

		fmt.Print(out)
		fmt.Printf("[%s] %s \n\n", cyan(m.output.CallingModule), green("End of loot file."))
	}

	fmt.Printf("[%s] Loot written to [%s]\n", cyan(m.output.CallingModule), pullFile)

}

func (m *SecretsModule) getSecretsManagerSecretsPerRegion(r string, dataReceiver chan Secret) {
	// "PaginationMarker" is a control variable used for output continuity, as AWS return the output in pages.
	var PaginationControl *string
	for {
		ListSecrets, err := m.SecretsManagerClient.ListSecrets(
			context.TODO(),
			&(secretsmanager.ListSecretsInput{
				NextToken: PaginationControl,
			}),
			func(o *secretsmanager.Options) {
				o.Region = r
			},
		)
		if err != nil {
			m.modLog.Error(err.Error())
			m.CommandCounter.Error++
			break
		}

		for _, secret := range ListSecrets.SecretList {
			name := aws.ToString(secret.Name)
			var description string
			if secret.Description != nil {
				description = aws.ToString(secret.Description)
			}

			dataReceiver <- Secret{
				AWSService:  "SecretsManager",
				Region:      r,
				Name:        name,
				Description: description,
			}

		}

		// The "NextToken" value is nil when there's no more data to return.
		if ListSecrets.NextToken != nil {
			PaginationControl = ListSecrets.NextToken
		} else {
			PaginationControl = nil
			break
		}
	}
}

func (m *SecretsModule) getSSMParametersPerRegion(r string, dataReceiver chan Secret) {
	// "PaginationMarker" is a control variable used for output continuity, as AWS return the output in pages.
	var PaginationControl *string

	for {
		DescribeParameters, err := m.SSMClient.DescribeParameters(
			context.TODO(),
			&(ssm.DescribeParametersInput{
				NextToken: PaginationControl,
			}),
			func(o *ssm.Options) {
				o.Region = r
			},
		)
		if err != nil {
			m.modLog.Error(err.Error())
			m.CommandCounter.Error++
			break
		}

		for _, parameter := range DescribeParameters.Parameters {
			var description string
			name := aws.ToString(parameter.Name)
			if parameter.Description != nil {
				description = aws.ToString(parameter.Description)
			}

			dataReceiver <- Secret{
				AWSService:  "SSM",
				Region:      r,
				Name:        name,
				Description: description,
			}

		}

		// The "NextToken" value is nil when there's no more data to return.
		if DescribeParameters.NextToken != nil {
			PaginationControl = DescribeParameters.NextToken
		} else {
			PaginationControl = nil
			break
		}
	}
}