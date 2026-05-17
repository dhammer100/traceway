<script lang="ts">
    import { goto } from '$app/navigation';

    type Tab = 'organization' | 'project';

    let { active }: { active: Tab } = $props();

    const tabs: { id: Tab; label: string; href: string }[] = [
        { id: 'organization', label: 'Organization', href: '/settings' },
        { id: 'project', label: 'Project', href: '/settings/project' }
    ];
</script>

<div class="border-b">
    <nav class="-mb-px flex gap-6" aria-label="Settings sections">
        {#each tabs as tab}
            <button
                type="button"
                class="border-b-2 px-1 py-2 text-sm font-medium transition-colors {active === tab.id ? 'border-primary text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'}"
                onclick={() => goto(tab.href)}
            >
                {tab.label}
            </button>
        {/each}
    </nav>
</div>
