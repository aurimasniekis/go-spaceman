// Adapted from Porch site/.vitepress/theme/index.ts (Apache-2.0, same author).
import DefaultTheme from 'vitepress/theme';
import type { Theme } from 'vitepress';

import Shot from './Shot.vue';
import './custom.css';

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('Shot', Shot);
  },
} satisfies Theme;
