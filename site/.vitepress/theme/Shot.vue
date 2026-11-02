<script setup lang="ts">
// Adapted from Porch site/.vitepress/theme/Shot.vue (Apache-2.0, same author).
import { withBase } from 'vitepress';

// A screenshot from public/screenshots (scripts/screenshots.sh): a -light (Light Owl) and a -dark (Night Owl) capture
// of the real UI that follow the site theme.
defineProps<{ name: string; alt: string; caption?: string }>();
</script>

<template>
  <figure class="sm-shot">
    <img class="sm-light" :src="withBase(`/screenshots/${name}-light.png`)" :alt="alt" loading="lazy" width="1440" height="900" />
    <img class="sm-dark" :src="withBase(`/screenshots/${name}-dark.png`)" :alt="alt" loading="lazy" width="1440" height="900" />
    <figcaption v-if="caption">{{ caption }}</figcaption>
  </figure>
</template>
