import { m } from '#lib/paraglide/messages.js';
import type { ContainerRegistry } from '#lib/types/docker.js';

type RegistryIdentity = Pick<ContainerRegistry, 'url' | 'registryType'>;

/** Human readable name for a registry, falling back to its URL. */
export function getRegistryDisplayName(registry: RegistryIdentity): string {
	if (registry.registryType === 'ecr') return m.amazon_ecr();
	const url = registry.url;
	if (!url || url === 'docker.io') return m.registry_docker_hub();
	if (url.includes('ghcr.io')) return m.registry_github_container_registry();
	if (url.includes('gcr.io')) return m.registry_google_container_registry();
	if (url.includes('quay.io')) return m.registry_quay_io();
	return url;
}

/** Select options for the registries that mirror the getarcaneapp images. */
export function arcaneImageRegistryOptions() {
	return [
		{ value: 'ghcr.io', label: m.registry_github_container_registry() },
		{ value: 'docker.io', label: m.registry_docker_hub(), badge: m.no_pull_limits() }
	];
}

function arcaneRegistryHost(registry: string): string {
	return registry === 'docker.io' ? 'docker.io' : 'ghcr.io';
}

export function arcaneToolsImage(registry: string): string {
	return `${arcaneRegistryHost(registry)}/getarcaneapp/tools:latest`;
}

export function arcaneTrivyDbImages(registry: string): string[] {
	const host = arcaneRegistryHost(registry);
	return [`${host}/getarcaneapp/trivy-db:2`, `${host}/getarcaneapp/trivy-java-db:1`, `${host}/getarcaneapp/trivy-checks:1`];
}

export function arcaneUpdateCheckImage(registry: string, isLocalEnvironment: boolean): string | null {
	if (registry === 'auto') return null;
	return `${arcaneRegistryHost(registry)}/getarcaneapp/${isLocalEnvironment ? 'manager' : 'agent'}`;
}

/** Strips the scheme and trailing slashes so a registry URL can be used as an image host. */
function normalizeRegistryHost(url: string): string {
	return url.replace(/^https?:\/\//, '').replace(/\/+$/, '');
}

/**
 * Splits a registry URL into its host and the repository namespace in its path.
 * Docker Hub's `/v1/` API path is not a namespace.
 */
export function splitRegistryUrl(url: string): { host: string; namespace: string } {
	const [host = '', ...path] = normalizeRegistryHost(url || 'docker.io').split('/');
	if (/^(index\.|registry-1\.)?docker\.io$/i.test(host) && path[0] === 'v1') path.shift();
	return { host, namespace: path.join('/') };
}

/**
 * Builds a `host/repository:tag` reference, or an empty string when the
 * repository name or tag is missing.
 */
export function buildImageReference(registryUrl: string, repositoryName: string, tag: string): string {
	const repository = repositoryName.trim();
	const trimmedTag = tag.trim();
	if (!repository || !trimmedTag) return '';
	return `${normalizeRegistryHost(registryUrl)}/${repository}:${trimmedTag}`;
}
