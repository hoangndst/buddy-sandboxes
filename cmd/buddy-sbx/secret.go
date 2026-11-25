package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

type secretSetOptions struct{ sandbox, baseURL, model, authType, repository string }

func runSecretSet(cmd *cobra.Command, requestedService string, options *secretSetOptions) error {
	service, descriptor, err := resolveSecretService(requestedService, options)
	if err != nil {
		return err
	}
	key, err := readSecretValue(descriptor)
	if err != nil {
		return err
	}
	defer func() { key = "" }()
	if options.baseURL == "" && descriptor.HostBaseURLEnvironment != "" {
		options.baseURL = strings.TrimSpace(os.Getenv(descriptor.HostBaseURLEnvironment))
	}
	if options.model == "" && descriptor.HostModelEnvironment != "" {
		options.model = strings.TrimSpace(os.Getenv(descriptor.HostModelEnvironment))
	}
	c, err := getClient()
	if err != nil {
		return err
	}
	if descriptor.Kind == credentials.ServiceKindGitRepository {
		err = c.SetGitCredential(cmd.Context(), options.repository, options.sandbox, key)
	} else {
		credentialOptions := client.CredentialOptions{
			BaseURL: options.baseURL, DefaultModel: options.model, AuthType: options.authType,
		}
		err = c.SetCredential(cmd.Context(), service, options.sandbox, key, credentialOptions)
	}
	if err != nil {
		return err
	}
	return writeSecretSetResult(cmd, service, descriptor, options)
}

func resolveSecretService(
	requestedService string, options *secretSetOptions,
) (string, credentials.ServiceDescriptor, error) {
	service, err := normalizeProvider(requestedService)
	if err != nil {
		return "", credentials.ServiceDescriptor{}, err
	}
	descriptor, _ := credentials.LookupService(service)
	if options.sandbox != "" && !descriptor.SupportsSandboxScope {
		return "", descriptor, fmt.Errorf("%s credentials do not support sandbox scope", descriptor.DisplayName)
	}
	if descriptor.RequiresRepository && options.repository == "" {
		return "", descriptor, fmt.Errorf("%s credentials require --repo", descriptor.DisplayName)
	}
	if !descriptor.RequiresRepository && options.repository != "" {
		return "", descriptor, fmt.Errorf("--repo is not valid for %s credentials", descriptor.DisplayName)
	}
	providerOptionsSet := options.baseURL != "" || options.model != "" || options.authType != ""
	if descriptor.Kind == credentials.ServiceKindGitRepository && providerOptionsSet {
		return "", descriptor, fmt.Errorf("provider options cannot be used with %s credentials", descriptor.DisplayName)
	}
	if options.baseURL != "" && !descriptor.SupportsCustomEndpoint {
		return "", descriptor, fmt.Errorf("%s credentials do not support --url", descriptor.DisplayName)
	}
	if options.model != "" && !descriptor.SupportsDefaultModel {
		return "", descriptor, fmt.Errorf("%s credentials do not support --model", descriptor.DisplayName)
	}
	if options.authType != "" && !descriptor.SupportsAuthType {
		return "", descriptor, fmt.Errorf("%s credentials do not support --auth-type", descriptor.DisplayName)
	}
	return service, descriptor, nil
}

func readSecretValue(descriptor credentials.ServiceDescriptor) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read %s credential: %w", descriptor.DisplayName, err)
		}
		if value := strings.TrimSpace(line); value != "" {
			return value, nil
		}
	}
	if descriptor.ValueEnvironment != "" {
		if value := strings.TrimSpace(os.Getenv(descriptor.ValueEnvironment)); value != "" {
			return value, nil
		}
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		label := descriptor.DisplayName + " credential"
		if descriptor.Kind == credentials.ServiceKindProvider {
			label = descriptor.DisplayName + " API key"
		}
		_, _ = fmt.Fprintf(os.Stderr, "Enter %s: ", label)
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read %s credential: %w", descriptor.DisplayName, err)
		}
		defer func() {
			for i := range value {
				value[i] = 0
			}
		}()
		if secret := strings.TrimSpace(string(value)); secret != "" {
			return secret, nil
		}
	}
	message := descriptor.DisplayName + " credential is empty; enter interactively or provide it on standard input"
	if descriptor.ValueEnvironment != "" {
		message += ", or set " + descriptor.ValueEnvironment
	}
	return "", fmt.Errorf("%s", message)
}

func writeSecretSetResult(
	cmd *cobra.Command, service string, descriptor credentials.ServiceDescriptor, options *secretSetOptions,
) error {
	if descriptor.Kind == credentials.ServiceKindGitRepository {
		if options.sandbox == "" {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stored %s credential for %s at user scope.\n",
				descriptor.DisplayName, options.repository)
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stored %s credential for %s at sandbox scope %s.\n",
			descriptor.DisplayName, options.repository, options.sandbox)
		return err
	}
	if options.sandbox == "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stored %s API key for your account.\n", service)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stored %s API key for sandbox %s.\n", service, options.sandbox)
	return err
}

func normalizeProvider(service string) (string, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	if _, ok := credentials.LookupService(service); !ok {
		return "", fmt.Errorf("unsupported credential service %q; choose from: %s",
			service, strings.Join(credentials.ServiceIDs(), ", "))
	}
	return service, nil
}
