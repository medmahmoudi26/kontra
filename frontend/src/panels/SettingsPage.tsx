/**
 * Settings: what this appliance is configured with, and what it will hold once the store exists.
 *
 * THE SURFACE COMES BEFORE ITS CONTENTS, ON PURPOSE. Secrets have never had a home here at all —
 * they live in `.env` files and a provisioned `worker.env` — and the thing that makes that
 * discoverable is a page that exists and says so. What ships in this slice is the frame plus the
 * one section that is already real; the secret store, the slot bindings and the resolution audit
 * are their own work.
 *
 * A SECTION THAT IS NOT BUILT SAYS WHAT IS MISSING RATHER THAN BEING ABSENT, and that is the whole
 * of the design here. An empty page teaches an operator that this console has no opinion about
 * credentials; a page that names the contract — write-only values, actors declaring slots, the
 * operator binding them — teaches what to expect and where their credentials are until then. The
 * one thing it must never do is draw a form that silently keeps nothing.
 *
 * NOTHING HERE IS A FIXTURE. There is no secret list to draw because there is no secret store, and
 * a plausible-looking one would be the worst possible thing to put on this page.
 *
 * PROPS IN, MARKUP OUT: {@link SettingsSurface} draws whatever sections it is handed, so every
 * state — none at all, one ready, one unbuilt — is a node test with no DOM.
 */

import type { ReactNode } from 'react';

import { useAppStore, type Theme } from '../state/store';
import SecretsSection from './SecretsSection';
import SlotBindingsSection from './SlotBindings';
import { Button } from '@/components/ui/button';

export interface SettingsSection {
  /** Stable, and the `data-testid` suffix. */
  id: string;
  title: string;
  /** One or two sentences: what this section is FOR. */
  blurb: string;
  /**
   * `ready` is a section an operator can act on right now. `unbuilt` is one whose backend does not
   * exist — it renders its {@link SettingsSection.needs} instead of a control, because a disabled
   * form is a thing people keep clicking.
   */
  state: 'ready' | 'unbuilt';
  /** Only for `unbuilt`: what has to exist first, and where the setting lives until it does. */
  needs?: string;
  /** Only for `ready`. */
  body?: ReactNode;
}

export function SettingsSurface({
  sections,
}: {
  sections: readonly SettingsSection[];
}): JSX.Element {
  return (
    <div className="dataset-page" data-testid="settings-page">
      <header className="flex flex-col gap-1">
        <h1 className="m-0 text-sm font-semibold">Settings</h1>
        <p className="m-0 text-xs text-muted-foreground">
          Secrets and configuration for this appliance. A secret’s value is write-only: set it, name
          it, rotate it, revoke it — never read it back.
        </p>
      </header>

      {sections.length === 0 ? (
        /* REACHABLE, AND NOT A BUG. A build with no configurable section is a legitimate appliance
           — and saying so is better than a header floating over nothing, which reads as a page
           that failed to load. */
        <p className="m-0 text-xs text-muted-foreground" data-testid="settings-empty">
          Nothing on this appliance is configurable from here yet.
        </p>
      ) : (
        <div className="flex flex-col gap-3">
          {sections.map((section) => (
            <section
              key={section.id}
              data-testid={`settings-${section.id}`}
              data-state={section.state}
              className="flex flex-col gap-2 rounded-lg border border-border bg-card p-3"
            >
              <div className="flex items-baseline justify-between gap-3">
                <h2 className="m-0 text-xs font-semibold">{section.title}</h2>
                {section.state === 'unbuilt' && (
                  /* NOT A DISABLED CONTROL, AND NOT A ROADMAP BADGE. It says the backend is absent,
                     which is a fact about this build rather than a promise about a later one. */
                  <span className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[10px] uppercase tracking-wide text-amber-300">
                    no store yet
                  </span>
                )}
              </div>
              <p className="m-0 text-[11px] text-muted-foreground">{section.blurb}</p>
              {section.state === 'unbuilt' ? (
                <p
                  className="m-0 text-[11px] text-muted-foreground"
                  data-testid={`settings-${section.id}-needs`}
                >
                  {section.needs}
                </p>
              ) : (
                section.body
              )}
            </section>
          ))}
        </div>
      )}
    </div>
  );
}

/** The theme picker. Two buttons rather than a toggle, because which one is current has to be
 *  readable without knowing what the icon on a toggle means. */
function ThemePicker({
  theme,
  onTheme,
}: {
  theme: Theme;
  onTheme(theme: Theme): void;
}): JSX.Element {
  return (
    <div className="flex gap-2" data-testid="settings-theme">
      {(['dark', 'light'] as const).map((t) => (
        <Button
          key={t}
          size="sm"
          variant={theme === t ? 'default' : 'outline'}
          data-testid={`settings-theme-${t}`}
          data-selected={theme === t ? 'true' : undefined}
          onClick={() => onTheme(t)}
        >
          {t}
        </Button>
      ))}
    </div>
  );
}

/**
 * The sections this build actually has.
 *
 * Appearance is here because it is the one piece of configuration this console already owns — it is
 * in the rail's corner today, which is a fine place for a toggle and a poor place to discover that
 * the setting exists.
 */
export default function SettingsPage(): JSX.Element {
  const theme = useAppStore((s) => s.theme);
  const setTheme = useAppStore((s) => s.setTheme);

  const sections: SettingsSection[] = [
    {
      id: 'secrets',
      title: 'Secrets',
      blurb:
        'Named, versioned references — never inlined. Workflow code names a secret and the worker resolves it at the last hop; an actor fetches its own at load, authenticated as itself. A value is never handed through a Batch, an activity argument or a workflow argument, because workflow history keeps all three in the clear.',
      state: 'ready',
      body: <SecretsSection />,
    },
    {
      id: 'bindings',
      title: 'Credential bindings',
      blurb:
        'Actors declare slots; you bind them. A third-party actor\u2019s author cannot know your secret names, so it declares `api_key` and you point that at whichever of your secrets it should be \u2014 and it never learns which. What an actor will ask for is listed before it runs, a new slot in a new version reads as a change, and every read and every refusal is in the ledger at the bottom.',
      state: 'ready',
      body: <SlotBindingsSection />,
    },
    {
      id: 'appearance',
      title: 'Appearance',
      blurb: 'Light or dark. Held in this browser, not on the appliance.',
      state: 'ready',
      body: <ThemePicker theme={theme} onTheme={setTheme} />,
    },
  ];

  return <SettingsSurface sections={sections} />;
}
