package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

func runUtilityCommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "migrate":
		return true, runMigrate(args[1:])
	case "bootstrap-admin":
		return true, runBootstrapAdmin(args[1:])
	case "recover-admin-token":
		return true, runRecoverAdminToken(args[1:])
	case "migrate-legacy-token":
		return true, runMigrateLegacyToken(args[1:])
	case "namespace":
		if len(args) > 1 && args[1] == "enable" {
			return true, runEnableNamespace(args[2:])
		}
		return true, fmt.Errorf("usage: buddy-api namespace enable --namespace <name>")
	default:
		return false, nil
	}
}

func connectIdentityStore(ctx context.Context) (*store.Postgres, *identity.Store, error) {
	database, err := store.Connect(ctx, os.Getenv("BUDDY_DATABASE_URL"))
	if err != nil {
		return nil, nil, err
	}
	return database, identity.NewStore(database.Pool), nil
}

func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("secret file path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("secret file is empty")
	}
	return value, nil
}

func runMigrate(args []string) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	bootstrapFile := set.String("bootstrap-token-file", "", "Optional one-time bootstrap token file to hash into the database")
	if err := set.Parse(args); err != nil {
		return err
	}
	database, users, err := connectIdentityStore(context.Background())
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		return err
	}
	if *bootstrapFile != "" {
		token, err := readSecretFile(*bootstrapFile)
		if err != nil {
			return err
		}
		if err := users.InitializeBootstrapToken(context.Background(), token); err != nil {
			return err
		}
	}
	return nil
}

func runBootstrapAdmin(args []string) error {
	set := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	displayName := set.String("display-name", "Administrator", "Initial administrator display name")
	namespace := set.String("namespace", "", "Previously enabled administrator namespace")
	tokenFile := set.String("bootstrap-token-file", "", "One-time bootstrap token file")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || *namespace == "" || *displayName == "" {
		return fmt.Errorf("usage: buddy-api bootstrap-admin --namespace <name> --bootstrap-token-file <path>")
	}
	token, err := readSecretFile(*tokenFile)
	if err != nil {
		return err
	}
	database, users, err := connectIdentityStore(context.Background())
	if err != nil {
		return err
	}
	defer database.Close()
	user, issued, err := users.BootstrapAdmin(context.Background(), token, *displayName, *namespace)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Created administrator %s (%s). Save the token now; it is shown once.\n", user.DisplayName, user.ID)
	_, _ = fmt.Fprintln(os.Stdout, issued.Value)
	return nil
}

func runRecoverAdminToken(args []string) error {
	set := flag.NewFlagSet("recover-admin-token", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	userID := set.String("user-id", "", "Administrator user ID")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || *userID == "" {
		return fmt.Errorf("usage: buddy-api recover-admin-token --user-id <id>")
	}
	database, users, err := connectIdentityStore(context.Background())
	if err != nil {
		return err
	}
	defer database.Close()
	issued, err := users.RecoverAdminToken(context.Background(), *userID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, issued.Value)
	return nil
}

func runMigrateLegacyToken(args []string) error {
	set := flag.NewFlagSet("migrate-legacy-token", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	displayName := set.String("display-name", "Administrator", "Initial administrator display name")
	namespace := set.String("namespace", "", "Namespace mapped to the legacy token")
	tokenFile := set.String("legacy-token-file", "", "File containing the old static token")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || *namespace == "" {
		return fmt.Errorf("usage: buddy-api migrate-legacy-token --namespace <name> --legacy-token-file <path>")
	}
	legacyToken, err := readSecretFile(*tokenFile)
	if err != nil {
		return err
	}
	database, users, err := connectIdentityStore(context.Background())
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.RequireCurrentSchema(context.Background()); err != nil {
		return err
	}
	user, issued, err := users.ImportLegacyAdmin(context.Background(), *displayName, *namespace, legacyToken)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Migrated identity to administrator %s (%s). Remove the old static token after recording the new token.\n", user.DisplayName, user.ID)
	_, _ = fmt.Fprintln(os.Stdout, issued.Value)
	return nil
}

func runEnableNamespace(args []string) error {
	set := flag.NewFlagSet("namespace enable", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	namespace := set.String("namespace", "", "Tenant namespace to enable")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || *namespace == "" {
		return fmt.Errorf("usage: buddy-api namespace enable --namespace <name>")
	}
	if problems := validation.IsDNS1123Label(*namespace); len(problems) != 0 {
		return fmt.Errorf("invalid Kubernetes namespace")
	}
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load operator kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("initialize Kubernetes client: %w", err)
	}
	ctx := context.Background()
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, *namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("tenant namespace must exist before it is enabled: %w", err)
	}
	labels := map[string]string{}
	for key, value := range ns.Labels {
		labels[key] = value
	}
	labels["sandboxes.buddy.io/tenant"] = "enabled"
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("encode tenant namespace labels: %w", err)
	}
	if _, err := clientset.CoreV1().Namespaces().Patch(ctx, *namespace, k8stypes.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label tenant namespace: %w", err)
	}
	bindings := clientset.RbacV1().RoleBindings(*namespace)
	binding, err := bindings.Get(ctx, "buddy-api-sandbox-access", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		binding = &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "buddy-api-sandbox-access", Namespace: *namespace},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "buddy-api", Namespace: "buddy-sandboxes-system"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "buddy-api-sandbox-access"},
		}
		if _, err := bindings.Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create API namespace RoleBinding: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read API namespace RoleBinding: %w", err)
	} else if !roleBindingMatches(binding) {
		return errors.New("existing buddy-api-sandbox-access RoleBinding has unexpected subjects or roleRef")
	}
	database, users, err := connectIdentityStore(ctx)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.RequireCurrentSchema(ctx); err != nil {
		return err
	}
	if err := users.EnableNamespace(ctx, *namespace); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Enabled namespace %s for Buddy Sandboxes.\n", *namespace)
	return nil
}

func roleBindingMatches(binding *rbacv1.RoleBinding) bool {
	return binding.RoleRef.Kind == "ClusterRole" && binding.RoleRef.Name == "buddy-api-sandbox-access" &&
		len(binding.Subjects) == 1 && binding.Subjects[0].Kind == "ServiceAccount" &&
		binding.Subjects[0].Name == "buddy-api" && binding.Subjects[0].Namespace == "buddy-sandboxes-system"
}
