/** Ignores a rejected audio promise: a closed or suspended context needs no recovery. */
const ignore = (): void => {
  /* noop */
};

/** A cleanup that removes nothing. */
const noCleanup = (): void => {};

/** Length of the looped white-noise buffer. */
const NOISE_BUFFER_SECONDS = 2;

/**
 * Lowpass cutoff. Lower = less high-frequency content, which the ear perceives
 * as quieter at equal amplitude (equal-loudness contours). 1500 Hz keeps enough
 * body for the audio device to register the stream while pushing the spectral
 * peak well below the 2-5 kHz band where the ear is most sensitive. Was
 * 6000 Hz: operators reported it as audible hiss on quiet headphones.
 */
const LOWPASS_HZ = 1500;

/**
 * Effective output level. The BROWSER tab-is-playing-audio detector watches
 * signal RMS, but the human ear weights perceived loudness via equal-loudness
 * contours. Narrowing the lowpass 6000 to 1500 Hz already drops broadband RMS by
 * ~12 dB, so the gain is raised above the original 0.001: the tab indicator (and
 * Bluetooth keep-alive) tracks RMS and needs it back, while the ear barely
 * notices because almost no energy sits in the sensitive 2-5 kHz band. 0.0012
 * (about -58 dBFS) lands at a stable RMS the browser reliably detects while
 * staying inaudible at normal listening volumes. The earlier 0.0002 + 1500 Hz
 * combo went silent enough for the tab to drop its audio-playing icon entirely.
 */
const GAIN = 0.0012;

/**
 * Silent broadband-ish noise generator that keeps the browser tab "audibly alive".
 *
 * Purpose: prevent Bluetooth headphones (and some laptop audio chipsets) from
 * suspending the output stream during long agent runs, which eats the first
 * second of any real notification beep.
 *
 * This class owns ONLY the runtime state (is the AudioContext currently
 * producing noise?). The user preference lives in the global rush.json on the
 * backend (Options.KeepAliveEnabled) and reaches the web UI via the `config`
 * WebSocket event; sync between preference and runtime happens in connectWS.
 *
 * Design: a singleton AudioContext + buffer-source loop, lazily built on
 * start(). White noise feeds a lowpass biquad ("soft hiss"), then a gain node
 * held at a very small constant, then the destination. A visibilitychange
 * handler resumes the AudioContext if the OS/browser suspended it while hidden.
 */
export class KeepAlive {
  private ctx: AudioContext | null = null;
  private source: AudioBufferSourceNode | null = null;
  private filter: BiquadFilterNode | null = null;
  private gainNode: GainNode | null = null;
  private running = false;
  private autoResumeListener: (() => void) | null = null;

  /**
   * Fills a mono buffer with white noise.
   *
   * @param audio - the context the buffer belongs to
   */
  private buildNoiseBuffer(audio: AudioContext): AudioBuffer {
    const len = audio.sampleRate * NOISE_BUFFER_SECONDS;
    const buf = audio.createBuffer(1, len, audio.sampleRate);
    const d = buf.getChannelData(0);
    for (let i = 0; i < len; i++) d[i] = Math.random() * 2 - 1;
    return buf;
  }

  /** Whether the noise loop is currently producing sound. */
  public isRunning(): boolean {
    return this.running;
  }

  /**
   * Starts the noise loop; false when the browser has no AudioContext or construction failed.
   *
   * AudioContext construction should happen inside a user-gesture handler
   * (modern autoplay policy). Without a recent gesture the context is
   * constructed "suspended"; the visibilitychange/click handlers upstream call
   * .resume() when one becomes available.
   */
  public start(): boolean {
    if (this.running) return true;
    const AC =
      window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
    if (!AC) return false;
    try {
      const ctx = new AC();
      this.ctx = ctx;
      this.source = ctx.createBufferSource();
      this.source.buffer = this.buildNoiseBuffer(ctx);
      this.source.loop = true;
      this.filter = ctx.createBiquadFilter();
      this.filter.type = "lowpass";
      this.filter.frequency.value = LOWPASS_HZ;
      this.gainNode = ctx.createGain();
      this.gainNode.gain.value = GAIN;
      this.source.connect(this.filter).connect(this.gainNode).connect(ctx.destination);
      this.source.start();
      this.running = true;
      return true;
    } catch {
      this.stop();
      return false;
    }
  }

  /** Stops the noise loop and releases the audio graph. */
  public stop(): void {
    if (!this.running && !this.ctx) return;
    try {
      this.source?.stop();
    } catch {
      /* noop */
    }
    this.source?.disconnect();
    this.filter?.disconnect();
    this.gainNode?.disconnect();
    this.ctx?.close().catch(ignore);
    this.source = null;
    this.filter = null;
    this.gainNode = null;
    this.ctx = null;
    this.running = false;
  }

  /** Resumes the AudioContext if the OS/browser suspended it in the background. */
  private resumeIfSuspended(): void {
    if (!this.running || !this.ctx) return;
    if (this.ctx.state === "suspended") {
      this.ctx.resume().catch(ignore);
    }
  }

  /**
   * Wires a visibilitychange listener that nudges the AudioContext back to "running".
   *
   * Repeated calls are idempotent; the returned cleanup removes the listener and
   * allows a later installation (e.g. after StrictMode effect replay). A stale
   * cleanup (or one for a duplicate installation) must not remove a listener
   * installed by a later owner.
   */
  public installAutoResume(): () => void {
    if (this.autoResumeListener) return noCleanup;
    const listener = (): void => this.resumeIfSuspended();
    this.autoResumeListener = listener;
    document.addEventListener("visibilitychange", listener);
    return () => {
      if (this.autoResumeListener !== listener) return;
      document.removeEventListener("visibilitychange", listener);
      this.autoResumeListener = null;
    };
  }

  /** Removes the visibilitychange listener, if installed. */
  public uninstallAutoResume(): void {
    if (!this.autoResumeListener) return;
    document.removeEventListener("visibilitychange", this.autoResumeListener);
    this.autoResumeListener = null;
  }
}
