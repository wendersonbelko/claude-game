/**
 * Sabres: sound effects, all synthesized with the Web Audio API (no asset files),
 * and the music bus (volume + analyser used by the visuals).
 */

const PENTA = [0, 2, 4, 7, 9, 12, 14, 16, 19, 21, 24, 26];

export class Sfx {
    constructor(ctx) {
        this.ctx = ctx;
        this.master = ctx.createGain();
        this.master.gain.value = 0.9;
        this.music = ctx.createGain();
        this.music.gain.value = 0.85;
        this.fx = ctx.createGain();
        this.fx.gain.value = 0.7;
        // gentle compressor so many effects at once never clip
        this.comp = ctx.createDynamicsCompressor();
        this.comp.threshold.value = -14;
        this.comp.ratio.value = 6;
        this.analyser = ctx.createAnalyser();
        this.analyser.fftSize = 512;
        this.analyser.smoothingTimeConstant = 0.62;
        this.music.connect(this.analyser);
        this.music.connect(this.master);
        this.fx.connect(this.master);
        this.master.connect(this.comp);
        this.comp.connect(ctx.destination);
        this.bins = new Uint8Array(this.analyser.frequencyBinCount);
        this.noiseBuf = this.makeNoise();
        this.last = {};
    }

    makeNoise() {
        const n = this.ctx.sampleRate;
        const buf = this.ctx.createBuffer(1, n, this.ctx.sampleRate);
        const d = buf.getChannelData(0);
        for (let i = 0; i < n; i++) d[i] = Math.random() * 2 - 1;
        return buf;
    }

    setVolumes(music, fx) {
        this.music.gain.value = music;
        this.fx.gain.value = fx;
    }

    /** Bass / mid / treble energy of the music, 0..1. */
    levels() {
        this.analyser.getByteFrequencyData(this.bins);
        const avg = (a, b) => {
            let s = 0;
            for (let i = a; i < b; i++) s += this.bins[i];
            return s / ((b - a) * 255);
        };
        return {bass: avg(1, 6), mid: avg(6, 40), high: avg(40, 120)};
    }

    throttle(key, ms) {
        const now = performance.now();
        if (now - (this.last[key] || 0) < ms) return false;
        this.last[key] = now;
        return true;
    }

    tone(freq, dur, {type = 'sine', gain = 0.3, slide = 0, delay = 0, attack = 0.004} = {}) {
        const c = this.ctx, t = c.currentTime + delay;
        const o = c.createOscillator(), g = c.createGain();
        o.type = type;
        o.frequency.setValueAtTime(freq, t);
        if (slide) o.frequency.exponentialRampToValueAtTime(Math.max(20, freq * slide), t + dur);
        g.gain.setValueAtTime(0.0001, t);
        g.gain.exponentialRampToValueAtTime(gain, t + attack);
        g.gain.exponentialRampToValueAtTime(0.0001, t + dur);
        o.connect(g).connect(this.fx);
        o.start(t);
        o.stop(t + dur + 0.03);
    }

    noise(dur, {freq = 3000, q = 1, type = 'bandpass', gain = 0.3, delay = 0, sweep = 0} = {}) {
        const c = this.ctx, t = c.currentTime + delay;
        const s = c.createBufferSource();
        s.buffer = this.noiseBuf;
        const f = c.createBiquadFilter();
        f.type = type;
        f.frequency.setValueAtTime(freq, t);
        if (sweep) f.frequency.exponentialRampToValueAtTime(Math.max(40, freq * sweep), t + dur);
        f.Q.value = q;
        const g = c.createGain();
        g.gain.setValueAtTime(gain, t);
        g.gain.exponentialRampToValueAtTime(0.0001, t + dur);
        s.connect(f).connect(g).connect(this.fx);
        s.start(t, Math.random() * 0.5);
        s.stop(t + dur + 0.03);
    }

    /** A block was sliced. hand: 0 blue, 1 red; q: 0..1 quality. */
    cut(hand, q = 0.6, combo = 0) {
        const base = hand === 0 ? 520 : 440;
        const step = Math.min(7, Math.floor(combo / 6));
        const f = base * 2 ** (PENTA[step % PENTA.length] / 12);
        this.noise(0.12, {freq: 5200, q: 0.8, gain: 0.32, sweep: 0.35});
        this.tone(f, 0.16, {type: 'triangle', gain: 0.22, slide: 1.5});
        this.tone(f * 0.5, 0.2, {type: 'sine', gain: 0.3, slide: 0.55});
        if (q > 0.85) this.tone(f * 3, 0.22, {type: 'sine', gain: 0.1, delay: 0.015});
    }

    bad() {
        this.tone(160, 0.22, {type: 'sawtooth', gain: 0.22, slide: 0.5});
        this.noise(0.14, {freq: 900, q: 0.6, gain: 0.2});
    }

    miss() {
        this.tone(110, 0.2, {type: 'sine', gain: 0.25, slide: 0.6});
    }

    wall() {
        if (!this.throttle('wall', 140)) return;
        this.noise(0.28, {freq: 600, q: 0.5, type: 'lowpass', gain: 0.5, sweep: 0.4});
        this.tone(70, 0.3, {type: 'square', gain: 0.18, slide: 0.6});
    }

    spin(i, total) {
        const f = 392 * 2 ** (PENTA[Math.min(i, PENTA.length - 1)] / 12);
        this.tone(f, 0.28, {type: 'sine', gain: 0.26});
        this.tone(f * 2, 0.22, {type: 'triangle', gain: 0.1});
        if (i === total - 1) this.spinDone();
    }

    spinDone() {
        [0, 4, 7, 12].forEach((s, k) => this.tone(523 * 2 ** (s / 12), 0.4, {type: 'triangle', gain: 0.2, delay: 0.07 * k}));
    }

    clash() {
        if (!this.throttle('clash', 110)) return;
        this.noise(0.1, {freq: 7000, q: 1.5, type: 'highpass', gain: 0.35});
        this.tone(1800, 0.12, {type: 'square', gain: 0.08, slide: 0.3});
        this.tone(95, 0.1, {type: 'sawtooth', gain: 0.1});
    }

    sync() {
        [0, 7, 12].forEach((s, k) => this.tone(660 * 2 ** (s / 12), 0.3, {type: 'triangle', gain: 0.16, delay: 0.03 * k}));
    }

    fuse() {
        this.noise(0.5, {freq: 400, q: 1.2, gain: 0.5, sweep: 12});
        [0, 4, 7, 12, 16].forEach((s, k) => this.tone(330 * 2 ** (s / 12), 0.5, {type: 'sawtooth', gain: 0.1, delay: 0.05 * k}));
    }

    combo(n) {
        const s = n >= 100 ? [0, 4, 7, 12, 16] : [0, 4, 7, 12];
        s.forEach((x, k) => this.tone(587 * 2 ** (x / 12), 0.25, {type: 'square', gain: 0.07, delay: 0.05 * k}));
    }

    multiplier() {
        this.tone(880, 0.2, {type: 'triangle', gain: 0.18, slide: 1.5});
    }

    count(n) {
        this.tone(n === 0 ? 1046 : 660, n === 0 ? 0.45 : 0.18, {type: 'square', gain: 0.12});
    }

    fail() {
        [0, -3, -7, -12].forEach((s, k) => this.tone(330 * 2 ** (s / 12), 0.5, {type: 'sawtooth', gain: 0.14, delay: 0.16 * k, slide: 0.9}));
    }

    victory() {
        [0, 4, 7, 12, 16, 19, 24].forEach((s, k) => this.tone(392 * 2 ** (s / 12), 0.55, {type: 'triangle', gain: 0.17, delay: 0.09 * k}));
    }

    uiClick() {
        this.tone(740, 0.08, {type: 'square', gain: 0.07});
    }
}
