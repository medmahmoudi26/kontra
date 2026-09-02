/** AG Grid one-time setup: register the community modules (v33+ requires it) and expose
 * a theme that follows the app's light/dark toggle via the Theming API. */
import {
  AllCommunityModule,
  ModuleRegistry,
  colorSchemeDark,
  colorSchemeLight,
  themeQuartz,
} from 'ag-grid-community';
import type { Theme } from '../state/store';

ModuleRegistry.registerModules([AllCommunityModule]);

const light = themeQuartz.withPart(colorSchemeLight);
const dark = themeQuartz.withPart(colorSchemeDark);

export const gridTheme = (theme: Theme) => (theme === 'dark' ? dark : light);
