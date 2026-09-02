import { WorkbenchFiles } from '@kontra/frontend';

/**
 * The aside this list lives in, at the width the workbench gives it (228px) and with the folder
 * header above it, so a card shows the list at the size an operator actually reads it — a full-width
 * file list is a shape this component never has.
 */
function Aside({
  name,
  version,
  path,
  children,
}: {
  name: string;
  version?: string;
  path: string;
  children: React.ReactNode;
}) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground" style={{ maxWidth: 260 }}>
        <div
          style={{ width: '100%', height: 300 }}
          className="flex flex-col overflow-hidden rounded border border-border"
        >
          <div className="shrink-0 border-b border-border px-3 py-2.5">
            <div className="flex items-baseline gap-1.5">
              <span className="min-w-0 truncate font-mono text-[12.5px] font-semibold">{name}</span>
              {version && (
                <span className="font-mono text-[9.5px] text-muted-foreground">v{version}</span>
              )}
            </div>
            <p className="m-0 mt-0.5 break-all font-mono text-[9.5px] leading-snug text-muted-foreground">
              {path}
            </p>
          </div>
          {children}
        </div>
      </div>
    </div>
  );
}

const noop = () => {};

/**
 * A Python Actor's folder, with `actor.py` open. All three files matter and all three are here:
 * `actor.py` is the code, `actor.json` is the manifest the SDKs read, and `description.md` is where
 * a Method's documentation comes from — an editor that could only open the marker would be an editor
 * for the least interesting of them.
 */
export function AnActorFolder() {
  return (
    <Aside name="probe" version="0.2.0" path="/srv/checkout/examples/python/probe">
      <WorkbenchFiles
        rows={[
          { name: 'actor.json', bytes: 240, dirty: false },
          { name: 'actor.py', bytes: 3646, dirty: false },
          { name: 'description.md', bytes: 612, dirty: false },
        ]}
        selected="actor.py"
        onSelect={noop}
      />
    </Aside>
  );
}

/**
 * THE ONE THING THIS LIST CARRIES THAT THE OPEN EDITOR CANNOT: `actor.py` holds edits nobody has
 * written while `description.md` is the file on screen. Switching files keeps every buffer, so a
 * folder can sit like this indefinitely — without the dot, the work is invisible until it is lost.
 */
export function UnsavedInAFileYouAreNotLookingAt() {
  return (
    <Aside name="probe" version="0.2.0" path="/srv/checkout/examples/python/probe">
      <WorkbenchFiles
        rows={[
          { name: 'actor.json', bytes: 240, dirty: false },
          { name: 'actor.py', bytes: 3646, dirty: true },
          { name: 'description.md', bytes: 612, dirty: false },
        ]}
        selected="description.md"
        onSelect={noop}
      />
    </Aside>
  );
}

/**
 * `description.md` started from the button under this list: it has a row so there is something to
 * click back to, and it says `new` rather than `0 B`. A size of zero would read as a file on disk
 * with nothing in it — the opposite of the truth, which is a file with a template in it and nothing
 * on disk.
 */
export function NotWrittenYet() {
  return (
    <Aside name="beacon" version="0.1.0" path="/srv/checkout/examples/python/beacon">
      <WorkbenchFiles
        rows={[
          { name: 'actor.json', bytes: 198, dirty: false },
          { name: 'actor.py', bytes: 1284, dirty: false },
          { name: 'description.md', bytes: null, dirty: true },
        ]}
        selected="description.md"
        onSelect={noop}
      />
    </Aside>
  );
}

/**
 * A Go Actor's folder — a folder holds more than Python, and the sizes span three orders of
 * magnitude, which is the whole reason `fmtBytes` prints bytes under a kilobyte instead of rounding
 * a 240-byte manifest to `0.0 KB`.
 */
export function AGoActorFolder() {
  return (
    <Aside name="nscheck" version="0.1.0" path="/srv/checkout/examples/go/nscheck">
      <WorkbenchFiles
        rows={[
          { name: 'actor.go', bytes: 7412, dirty: false },
          { name: 'actor.json', bytes: 266, dirty: false },
          { name: 'description.md', bytes: 1893, dirty: false },
          { name: 'go.mod', bytes: 412, dirty: false },
          { name: 'go.sum', bytes: 28914, dirty: false },
        ]}
        selected="actor.go"
        onSelect={noop}
      />
    </Aside>
  );
}

/**
 * Nothing the editor can open, and it says which rule excluded everything rather than drawing an
 * empty box. A folder of dotfiles and subdirectories is a real and recoverable situation; a blank
 * panel is indistinguishable from a read that never came back.
 */
export function NothingReadable() {
  return (
    <Aside name="crawl4ai" version="0.3.0" path="/srv/checkout/examples/python/crawl4ai">
      <WorkbenchFiles rows={[]} selected="" onSelect={noop} />
    </Aside>
  );
}
