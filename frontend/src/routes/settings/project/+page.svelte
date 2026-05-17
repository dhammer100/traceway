<script lang="ts">
    import { goto } from '$app/navigation';
    import { authState } from '$lib/state/auth.svelte';
    import { projectsState, getFrameworkLabel } from '$lib/state/projects.svelte';
    import { LoadingCircle } from '$lib/components/ui/loading-circle';
    import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '$lib/components/ui/card';
    import { Label } from '$lib/components/ui/label';
    import { Button } from '$lib/components/ui/button';
    import SettingsTabs from '../settings-tabs.svelte';
    import { Copy, Check, Eye, EyeOff, RefreshCw, CircleAlert } from 'lucide-svelte';
    import { toast } from 'svelte-sonner';
    import * as AlertDialog from '$lib/components/ui/alert-dialog';
    import * as Alert from '$lib/components/ui/alert';

    const project = $derived(projectsState.currentProject);
    const currentOrganizationId = $derived(project?.organizationId);

    const hasAccess = $derived(
        currentOrganizationId !== null &&
        currentOrganizationId !== undefined &&
        authState.canManageOrganization(currentOrganizationId)
    );

    $effect(() => {
        if (!hasAccess) {
            goto('/');
        }
    });

    let tokenRevealed = $state(false);
    let copiedId = $state(false);
    let copiedToken = $state(false);
    let showRotateDialog = $state(false);
    let rotating = $state(false);
    let rotateError = $state('');

    async function rotateToken() {
        rotating = true;
        rotateError = '';
        try {
            await projectsState.rotateToken();
            tokenRevealed = true;
            showRotateDialog = false;
            toast.success('Successfully rotated the API token', { position: 'top-center' });
        } catch (e: unknown) {
            rotateError = e instanceof Error ? e.message : 'Failed to rotate token';
        } finally {
            rotating = false;
        }
    }

    async function copyText(value: string, setter: (v: boolean) => void) {
        await navigator.clipboard.writeText(value);
        setter(true);
        toast.success('Copied to clipboard', { position: 'top-center' });
        setTimeout(() => setter(false), 2000);
    }

    function maskToken(token: string): string {
        if (token.length <= 8) return '••••••••';
        return token.slice(0, 4) + '•'.repeat(token.length - 8) + token.slice(-4);
    }
</script>

<div class="space-y-6">
    <div>
        <h1 class="text-2xl font-semibold tracking-tight">Settings</h1>
    </div>

    <SettingsTabs active="project" />

    {#if !project}
        <div class="flex items-center justify-center py-12">
            <LoadingCircle size="xlg" />
        </div>
    {:else}
        <div class="space-y-6">
            <Card>
                <CardHeader>
                    <CardTitle>Project Details</CardTitle>
                    <CardDescription>Information about your project</CardDescription>
                </CardHeader>
                <CardContent class="space-y-4">
                    <div class="grid gap-4">
                        <div class="grid grid-cols-4 items-center gap-4">
                            <Label class="text-right text-muted-foreground">Name</Label>
                            <div class="col-span-3">{project.name}</div>
                        </div>
                        <div class="grid grid-cols-4 items-center gap-4">
                            <Label class="text-right text-muted-foreground">Project ID</Label>
                            <div class="col-span-3 flex items-center gap-2">
                                <code class="rounded bg-muted px-2 py-1 text-sm font-mono">{project.id}</code>
                                <Button variant="ghost" size="sm" onclick={() => copyText(project.id, v => (copiedId = v))}>
                                    {#if copiedId}
                                        <Check class="size-4" />
                                    {:else}
                                        <Copy class="size-4" />
                                    {/if}
                                </Button>
                            </div>
                        </div>
                        <div class="grid grid-cols-4 items-center gap-4">
                            <Label class="text-right text-muted-foreground">Framework</Label>
                            <div class="col-span-3">{getFrameworkLabel(project.framework)}</div>
                        </div>
                        <div class="grid grid-cols-4 items-center gap-4">
                            <Label class="text-right text-muted-foreground">Backend URL</Label>
                            <div class="col-span-3">
                                <code class="rounded bg-muted px-2 py-1 text-sm font-mono">{project.backendUrl}</code>
                            </div>
                        </div>
                    </div>
                </CardContent>
            </Card>

            <Card>
                <CardHeader>
                    <CardTitle>API Token</CardTitle>
                    <CardDescription>
                        Used by SDKs (<code class="text-xs">Authorization: Bearer &lt;token&gt;</code>) and webhook integrations. Anyone with this token can write telemetry into the project.
                    </CardDescription>
                </CardHeader>
                <CardContent class="space-y-4">
                    <div class="grid grid-cols-4 items-center gap-4">
                        <Label class="text-right text-muted-foreground">Token</Label>
                        <div class="col-span-3 flex items-center gap-2">
                            <code class="rounded bg-muted px-2 py-1 text-sm font-mono break-all">
                                {tokenRevealed ? project.token : maskToken(project.token)}
                            </code>
                            <Button variant="ghost" size="sm" onclick={() => (tokenRevealed = !tokenRevealed)}>
                                {#if tokenRevealed}
                                    <EyeOff class="size-4" />
                                {:else}
                                    <Eye class="size-4" />
                                {/if}
                            </Button>
                            <Button variant="ghost" size="sm" onclick={() => copyText(project.token, v => (copiedToken = v))}>
                                {#if copiedToken}
                                    <Check class="size-4" />
                                {:else}
                                    <Copy class="size-4" />
                                {/if}
                            </Button>
                        </div>
                    </div>
                    <div class="grid grid-cols-4 items-center gap-4">
                        <div></div>
                        <div class="col-span-3">
                            <Button variant="outline" size="sm" onclick={() => (showRotateDialog = true)}>
                                <RefreshCw class="mr-1 size-4" /> Rotate Token
                            </Button>
                            <p class="text-xs text-muted-foreground mt-1">
                                Issues a new token and invalidates the current one immediately.
                            </p>
                        </div>
                    </div>
                </CardContent>
            </Card>
        </div>
    {/if}
</div>

<AlertDialog.Root
    bind:open={showRotateDialog}
    onOpenChange={(o) => {
        if (!o) rotateError = '';
    }}
>
    <AlertDialog.Content interactOutsideBehavior="close">
        <AlertDialog.Header>
            <AlertDialog.Title>Rotate API Token</AlertDialog.Title>
            <AlertDialog.Description>
                The current token will stop working immediately. Any SDK, webhook integration, or script using it will start receiving 401 Unauthorized until reconfigured with the new token. This cannot be undone.
            </AlertDialog.Description>
        </AlertDialog.Header>
        {#if rotateError}
            <Alert.Root variant="destructive" class="border-red-200 bg-red-50">
                <CircleAlert class="h-4 w-4 text-red-700" />
                <Alert.Title class="text-red-800">Error</Alert.Title>
                <Alert.Description class="text-red-700">{rotateError}</Alert.Description>
            </Alert.Root>
        {/if}
        <AlertDialog.Footer>
            <Button variant="outline" onclick={() => (showRotateDialog = false)} disabled={rotating}>Cancel</Button>
            <Button variant="destructive" onclick={rotateToken} disabled={rotating}>
                {#if rotating}
                    Rotating...
                {:else}
                    <RefreshCw class="mr-1 size-4" /> Rotate Token
                {/if}
            </Button>
        </AlertDialog.Footer>
    </AlertDialog.Content>
</AlertDialog.Root>
