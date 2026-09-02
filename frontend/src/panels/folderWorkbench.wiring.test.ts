/**
 * The workbench, WIRED — the two reads it makes, the switch that must clear them, and the three
 * outcomes of a serve.
 *
 * `folderWorkbench.ts` is a state machine with its own test, and `folderWorkbench.render.test.ts`
 * draws the file list from a literal. Neither can say whether the component ASKS for the right
 * things: it makes two independent reads (the folder's listing, then the selected file's bytes),
 * the second is keyed on a buffer it may already hold, and both are fenced by a `live` flag so a
 * folder switch does not paint the previous Actor's code under this one's name. That is the shape
 * of bug the file's own header is about, and none of it exists outside an effect.
 *
 * THE SERVE IS THE OTHER HALF, and it is three outcomes rather than two: it served, it refused, or
 * a worker is already there — the last arriving as a 409 that is answered with a BUTTON, because
 * reporting the desired end state in red is the version of this screen that sends an operator to
 * the terminal.
 */

import { createElement } from 'react';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ActorServeResult, Source, SourceFile } from '../run/api';

const fetchSourceFiles = vi.fn();
const fetchSourceFile = vi.fn();
const serveActorSource = vi.fn();

vi.mock('../run/api', async (actual) => {
  const real = await actual<typeof import('../run/api')>();
  return {
    ...real,
    fetchSourceFiles: (...a: unknown[]) => fetchSourceFiles(...a),
    fetchSourceFile: (...a: unknown[]) => fetchSourceFile(...a),
    serveActorSource: (...a: unknown[]) => serveActorSource(...a),
  };
});

/** The read-only viewer, stubbed — it is CodeMirror, and what is under test is which bytes reach it. */
vi.mock('@uiw/react-codemirror', () => ({
  default: ({ value }: { value: string }) =>
    createElement('pre', { 'data-testid': 'workbench-editor' }, value),
}));

/**
 * xterm reaches for a canvas jsdom does not implement, and the pane is a screen, not a decision.
 *
 * IT KEEPS THE `session ?? derived` RULE VISIBLE, because that is what is under test on this
 * screen: the workbench hands the pane the session a serve in THIS visit reported and, separately,
 * the name it derived from the folder — and the second is not a fallback for a failure, it is the
 * ordinary case where the worker was started an hour ago or from a terminal.
 */
vi.mock('./WorkerPane', () => ({
  WorkerPane: ({ session, derived }: { session?: string | null; derived?: string }) =>
    createElement('div', {
      'data-testid': 'worker-pane',
      'data-session': session ?? derived,
      'data-derived': derived,
    }),
}));

const { AlreadyServingError } = await import('../run/api');
const { FolderWorkbench } = await import('./FolderWorkbench');

function folder(over: Partial<Source> = {}): Source {
  return {
    id: 'at:/root/checkout/probe',
    kind: 'actor',
    name: 'probe',
    path: '/root/checkout/probe',
    version: '0.1.0',
    description: '',
    registeredAt: 1,
    ...over,
  };
}

function file(name: string, bytes = 100): SourceFile {
  return { name, bytes, modifiedAt: 1 };
}

const FILES = [file('actor.json', 64), file('actor.py', 2048), file('description.md', 120)];

const SERVED: ActorServeResult = {
  actor: 'probe',
  version: '0.1.0',
  path: '/root/checkout/probe',
  session: 'probe-0_1_0',
  attach: 'tmux attach -t probe-0_1_0',
};

const onClose = vi.fn();

async function settle(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

async function open(source = folder()): Promise<ReturnType<typeof render>> {
  const view = render(createElement(FolderWorkbench, { source, onClose }));
  await settle();
  return view;
}

beforeEach(() => {
  localStorage.clear();
  vi.clearAllMocks();
  fetchSourceFiles.mockResolvedValue({ path: '/root/checkout/probe', files: FILES });
  fetchSourceFile.mockImplementation((_kind: string, _id: string, name: string) =>
    Promise.resolve({ name, source: `# ${name}\n` })
  );
  serveActorSource.mockResolvedValue(SERVED);
});

describe('the two reads', () => {
  it('lists the folder, then reads the file it decided to open', async () => {
    await open();
    expect(fetchSourceFiles).toHaveBeenCalledWith('actor', 'at:/root/checkout/probe');
    // THE CODE, not the manifest: `actor.json` sorts first and is never the file anyone opened the
    // folder to read. `firstToOpen` decides it; only a mounted component proves it is what is read.
    expect(fetchSourceFile).toHaveBeenCalledWith('actor', 'at:/root/checkout/probe', 'actor.py');
    expect(screen.getByTestId('workbench-editor').textContent).toBe('# actor.py\n');
    expect(screen.getByTestId('workbench-open-file').textContent).toContain('actor.py');
  });

  it('reads a file once and keeps its buffer across a switch back', async () => {
    await open();
    fireEvent.click(screen.getByTestId('workbench-file-description.md'));
    await settle();
    expect(fetchSourceFile).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByTestId('workbench-file-actor.py'));
    await settle();
    // The buffer is held, so no second read — which is what makes an unsaved edit survive a look at
    // another file.
    expect(fetchSourceFile).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId('workbench-editor').textContent).toBe('# actor.py\n');
  });

  it('says what a refused listing said, rather than drawing an empty folder', async () => {
    fetchSourceFiles.mockRejectedValue(new Error('the folder is not on this disk any more'));
    await open();
    expect(screen.getByTestId('workbench-error').textContent).toContain('not on this disk');
  });

  it('re-lists on demand', async () => {
    await open();
    fireEvent.click(screen.getByTestId('workbench-reload'));
    await settle();
    expect(fetchSourceFiles).toHaveBeenCalledTimes(2);
  });
});

describe('a different folder', () => {
  it('keeps nothing of the last one — not the buffers, not the serve report', async () => {
    const view = await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    expect(screen.getByTestId('actor-attach').textContent).toBe('tmux attach -t probe-0_1_0');
    expect(screen.getByTestId('actor-serve-report').textContent).toContain('/root/checkout/probe');

    // The new folder's reads never answer, so what is on screen is whatever the switch left behind —
    // and `probe`'s session beside `beacon`'s code is the one thing here an operator would copy
    // into a `tmux attach`.
    fetchSourceFiles.mockReturnValue(new Promise(() => {}));
    fetchSourceFile.mockReturnValue(new Promise(() => {}));
    view.rerender(
      createElement(FolderWorkbench, {
        source: folder({ id: 'at:/root/checkout/beacon', name: 'beacon' }),
        onClose,
      })
    );
    await settle();
    // The console still stands — it is how the new folder is served — but everything the LAST
    // serve reported is gone: the attach line falls back to the derived name and the `from` path
    // that only a real serve carries is not there.
    expect(screen.getByTestId('actor-attach').textContent).toBe('tmux attach -t beacon-0_1_0');
    expect(screen.getByTestId('actor-serve-report').textContent).not.toContain('/root/checkout/probe');
    expect(screen.queryByTestId('workbench-editor')).toBeNull();
    expect(fetchSourceFiles).toHaveBeenLastCalledWith('actor', 'at:/root/checkout/beacon');
  });

  it('names the pane after the folder even when nothing was served from here', async () => {
    // The DERIVED session is not a fallback for a failed serve — it is the case where the worker was
    // started an hour ago, or from a terminal. Without it the workbench draws an empty rectangle
    // beside a worker that is polling perfectly well.
    await open(folder({ name: 'probe', version: '0.2.0' }));
    const pane = screen.getByTestId('worker-pane');
    expect(pane.dataset.derived).toBe('probe-0_2_0');
    // The dots become underscores because TMUX rewrites them at creation — looking for
    // `probe-0.2.0` finds nothing while the pane sits there under the mangled spelling.
    expect(pane.dataset.session).toBe('probe-0_2_0');
  });

  it('lets the serve’s own answer win once there is one', async () => {
    serveActorSource.mockResolvedValue({ ...SERVED, session: 'probe-from-the-server' });
    await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    expect(screen.getByTestId('worker-pane').dataset.session).toBe('probe-from-the-server');
  });
});

describe('the three outcomes of a serve', () => {
  it('reports the session and the attach line it can be reached by', async () => {
    await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    expect(serveActorSource).toHaveBeenCalledWith('at:/root/checkout/probe', { restart: false });
    expect(screen.getByTestId('actor-attach').textContent).toContain('tmux attach -t probe-0_1_0');
    expect(screen.queryByTestId('actor-serve-error')).toBeNull();
  });

  it('answers a 409 with a BUTTON, not with red text', async () => {
    serveActorSource.mockRejectedValue(new AlreadyServingError('a worker is already serving probe'));
    await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    expect(screen.getByTestId('actor-serve-serving').textContent).toContain('already serving');
    // A worker being there is the state the operator wanted; calling it a failure sends them to a
    // terminal to fix nothing.
    expect(screen.queryByTestId('actor-serve-error')).toBeNull();
    expect(screen.getByTestId('actor-restart-button')).toBeTruthy();
  });

  it('restarts on the second press, and says so with the same call plus one flag', async () => {
    serveActorSource.mockRejectedValueOnce(new AlreadyServingError('a worker is already serving'));
    await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    fireEvent.click(screen.getByTestId('actor-restart-button'));
    await settle();
    expect(serveActorSource).toHaveBeenLastCalledWith('at:/root/checkout/probe', { restart: true });
    expect(screen.getByTestId('actor-attach').textContent).toBe('tmux attach -t probe-0_1_0');
    expect(screen.queryByTestId('actor-serve-serving')).toBeNull();
  });

  it('carries the CLI’s own last lines through a real refusal', async () => {
    serveActorSource.mockRejectedValue(
      new Error('serve failed (exit 1): ModuleNotFoundError: No module named "httpx"')
    );
    await open();
    fireEvent.click(screen.getByTestId('actor-serve-button'));
    await settle();
    expect(screen.getByTestId('actor-serve-error').textContent).toContain('No module named');
    // A refusal clears whatever a previous serve reported: no `from` path, and the attach line is
    // the derived name again rather than a server string about a worker that is not there.
    expect(screen.getByTestId('actor-attach').textContent).toBe('tmux attach -t probe-0_1_0');
    expect(screen.queryByTestId('actor-serve-serving')).toBeNull();
  });
});

describe('leaving', () => {
  it('closes back to the list it was opened from', async () => {
    await open();
    fireEvent.click(screen.getByTestId('workbench-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
