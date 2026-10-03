"""Synthesize the promo soundtrack (62 s), synced to the cuts in index.html.

Warm lo-fi groove at 100 BPM: FM electric piano, sub bass, soft drums, a pad
underneath, room reverb, plus UI clicks and two chimes on the "it works" beats.
Usage: python3 music.py out/music.wav   (then loudness-normalize with ffmpeg)
"""
import sys, wave
import numpy as np

SR, DUR = 44100, 66.0
N = int(SR * DUR)
BEAT = 60 / 100
BAR = BEAT * 4
DROP = 6.0                  # logo reveal: groove starts here
# index.html splices a 4 s host-install beat in at 11 s; cue times below are
# written on the original timeline and shifted past it.
INS_AT, INS = 11.0, 4.0
def at(x):
    return x if x < INS_AT else x + INS
OUTRO = at(56.4)            # end card: drums drop out
CLICKS = [at(x) for x in (13.05, 14.95, 16.65, 18.4, 23.6)]
CHIMES = [13.7, at(27.8), at(40.1)]   # console opens, guest in Claude, tests pass
CUTS = [11.0] + [at(x) for x in (11.0, 22.0, 32.0, 44.0, 52.0)]
FULL_AUTO = at(44.0)
rng = np.random.default_rng(7)


def hz(m):
    return 440 * 2 ** ((m - 69) / 12)


def band(x, lo=0, hi=SR / 2):
    """Zero-phase FFT band filter with soft edges."""
    X = np.fft.rfft(x)
    f = np.fft.rfftfreq(len(x), 1 / SR)
    g = np.ones_like(f)
    if lo:
        g *= 1 / (1 + (lo / np.maximum(f, 1)) ** 4)
    if hi < SR / 2:
        g *= 1 / (1 + (f / hi) ** 4)
    return np.fft.irfft(X * g, len(x))


def add(bus, start, sig):
    i = int(start * SR)
    if i >= len(bus) or i + len(sig) <= 0:
        return
    j = min(len(bus), i + len(sig))
    bus[max(i, 0):j] += sig[max(0, -i):j - i]


def seg(sec):
    return np.arange(int(sec * SR)) / SR


def ep(f, dur, vel=1.0):
    """FM electric piano note."""
    t = seg(dur + 1.2)
    idx = 2.4 * np.exp(-t * 5) + 0.35
    mod = idx * np.sin(2 * np.pi * f * t)
    tone = np.sin(2 * np.pi * f * t + mod) + 0.18 * np.sin(2 * np.pi * 2 * f * t) * np.exp(-t * 6)
    env = (1 - np.exp(-t * 300)) * np.exp(-t * 1.3) * np.clip((dur + 1.2 - t) / 0.4, 0, 1)
    return tone * env * vel


def chord_ep(notes, dur, vel):
    return sum(ep(hz(n), dur, vel * (0.85 if i else 1)) for i, n in enumerate(notes)) / len(notes)


# D major: Dmaj9 – Bm9 – Gmaj9 – A6sus (voicings around middle C)
CHORDS = [
    (38, [54, 57, 61, 64]),   # D:  F# A C# E
    (35, [54, 57, 61, 62]),   # Bm: F# A C# D
    (31, [54, 57, 59, 62]),   # G:  F# A B D
    (33, [52, 55, 57, 62]),   # A:  E G A D
]

ep_bus, pad_bus, bass_bus, drum_bus, fx_bus, lead_bus = (np.zeros(N) for _ in range(6))

# ---- harmony: bars anchored so a bar starts exactly on the drop ----
first = DROP - 3 * BAR
bar_starts = np.arange(first, DUR, BAR)
for b, t0 in enumerate(bar_starts):
    root, notes = CHORDS[b % 4]
    if t0 >= OUTRO - 0.01:
        break
    intro = t0 < DROP
    # electric piano: on 1 and the "and" of 2 (sparser in the intro)
    add(ep_bus, t0, chord_ep(notes, BEAT * 1.4, 0.9))
    if not intro:
        add(ep_bus, t0 + BEAT * 1.5, chord_ep(notes, BEAT * 1.0, 0.55))
        add(ep_bus, t0 + BEAT * 3.0, chord_ep(notes[1:], BEAT * 0.8, 0.4))
    # pad: detuned soft saws, filtered later
    t = seg(BAR + 0.6)
    env = np.clip(t / 0.5, 0, 1) * np.clip((BAR + 0.6 - t) / 0.6, 0, 1)
    p = np.zeros_like(t)
    for n in notes:
        for d in (-0.08, 0.08):
            f = hz(n) * (1 + d / 100)
            p += sum(np.sin(2 * np.pi * f * k * t) / k for k in (1, 2, 3))
    add(pad_bus, t0, p * env / 24)
    # bass: root on 1, the "and" of 2, and 4 — groove only
    if not intro:
        for off, l in ((0, 1.2), (1.5, 0.45), (3.0, 0.8)):
            t = seg(BEAT * l + 0.1)
            f = hz(root)
            s = np.sin(2 * np.pi * f * t) + 0.25 * np.sin(4 * np.pi * f * t)
            env = (1 - np.exp(-t * 400)) * np.exp(-t * 2.2) * np.clip((BEAT * l + 0.1 - t) / 0.05, 0, 1)
            add(bass_bus, t0 + off * BEAT, s * env)

# pluck motif an octave up, from the first product scene on (two-bar phrase)
MOTIF = [(0, 78), (0.75, 81), (1.5, 85), (2.5, 83), (3.5, 81), (4.5, 78), (5.25, 76), (6, 78)]
SHIFT = [0, -3, -7, -5]  # follow the chord roots
for b, t0 in enumerate(bar_starts):
    if t0 < CUTS[0] - BAR or t0 >= OUTRO - 0.01 or b % 2:
        continue
    for off, n in MOTIF:
        sh = SHIFT[(b + int(off // 4)) % 4]
        t = seg(0.9)
        f = hz(n + sh)
        tone = np.sin(2 * np.pi * f * t + 0.8 * np.sin(2 * np.pi * f * 2 * t) * np.exp(-t * 12))
        add(lead_bus, t0 + off * BEAT, tone * (1 - np.exp(-t * 500)) * np.exp(-t * 4.5) * 0.5)

# final chord rings out over the end card
add(ep_bus, OUTRO, chord_ep([50, 54, 57, 61, 64], 3.5, 1.0))
add(bass_bus, OUTRO, np.sin(2 * np.pi * hz(38) * seg(3)) * np.exp(-seg(3) * 1.8) * 0.5)

# ---- drums (from the drop until the end card) ----
kick_env = np.zeros(N)
noise = rng.standard_normal(int(0.5 * SR))
hat_src = band(noise, lo=7000)
snr_src = band(noise, lo=900, hi=5000)
for k in np.arange(DROP, OUTRO - 0.01, BEAT):
    i = round((k - DROP) / BEAT)
    pos = i % 4
    if pos in (0, 2):
        t = seg(0.45)
        add(drum_bus, k, np.sin(2 * np.pi * (58 * t + 110 / 30 * (1 - np.exp(-t * 30)))) * np.exp(-t * 9) * 0.55)
        add(kick_env, k, np.exp(-seg(0.35) * 9))
    if pos in (1, 3):
        t = seg(0.3)
        add(drum_bus, k, snr_src[:len(t)] * np.exp(-t * 16) * 0.38)
    busy = k >= FULL_AUTO  # full-auto section gets 16th hats
    steps = 4 if busy else 2
    for s in range(steps):
        sw = (0.06 if s % 2 else 0) * BEAT if not busy else 0
        t = seg(0.06)
        vel = (0.40 if s == 0 else 0.26) * (1.0 if busy else 0.9)
        add(drum_bus, k + s * BEAT / steps + sw, hat_src[:len(t)] * np.exp(-t * 70) * vel)

# riser into the drop, soft swish on each cut
t = seg(1.2)
add(fx_bus, DROP - 1.2, band(rng.standard_normal(len(t)), lo=2000, hi=9000) * (t / 1.2) ** 3 * 0.25)
for c in CUTS:
    t = seg(0.7)
    sw = band(rng.standard_normal(len(t)), lo=1500, hi=7000)
    add(fx_bus, c - 0.35, sw * np.sin(np.pi * t / 0.7) ** 2 * 0.08)
# UI clicks
for c in CLICKS:
    t = seg(0.05)
    add(fx_bus, c, (np.sin(2 * np.pi * 2400 * t) * 0.5 + band(rng.standard_normal(len(t)), lo=3000)) * np.exp(-t * 150) * 0.25)
# chimes on success beats
for c in CHIMES:
    for j, n in enumerate((81, 88)):
        t = seg(1.6)
        f = hz(n)
        add(fx_bus, c + j * 0.09, np.sin(2 * np.pi * f * t + 1.2 * np.sin(2 * np.pi * f * 3.5 * t) * np.exp(-t * 6)) * np.exp(-t * 3.2) * 0.13)

# ---- mix ----
duck = 1 - 0.35 * np.clip(kick_env, 0, 1)
pad = band(pad_bus, lo=220, hi=2400) * duck
epb = band(ep_bus, lo=140, hi=9000) * duck
dry = epb * 0.7 + pad * 0.3 + band(bass_bus, lo=38, hi=500) * 0.2 + band(drum_bus, lo=40) * 0.7 + band(lead_bus, lo=300) * 0.3 + fx_bus

# stereo room reverb (FFT convolution with decaying noise)
ir_t = seg(2.2)
send = epb * 0.5 + pad * 0.4 + drum_bus * 0.15 + fx_bus * 0.6 + lead_bus * 0.6
wet = []
for ch in range(2):
    ir = band(rng.standard_normal(len(ir_t)), lo=200, hi=5000) * np.exp(-ir_t * 3.0)
    ir /= np.sqrt(np.sum(ir ** 2))
    L = N + len(ir)
    wet.append(np.fft.irfft(np.fft.rfft(send, L) * np.fft.rfft(ir, L), L)[:N])
width = band(epb, lo=300) * 0.06  # a touch of EP width
left = dry + wet[0] * 0.28 + width
right = dry + wet[1] * 0.28 - width

fade = np.clip(seg(DUR) / 1.5, 0, 1) * np.clip((DUR - seg(DUR)) / 2.0, 0, 1)
fade = fade[:N]
def presence(x):
    """Master EQ: +5 dB around 2.5–7 kHz so the mix doesn't sound muffled."""
    X = np.fft.rfft(x)
    f = np.fft.rfftfreq(len(x), 1 / SR)
    g = 1 + 0.8 * np.exp(-((np.log2(np.maximum(f, 1) / 4200)) ** 2) / 0.8)
    return np.fft.irfft(X * g, len(x))


left, right = presence(left), presence(right)
mix = np.stack([left, right], axis=1) * fade[:, None]
mix = np.tanh(mix / np.max(np.abs(mix)) * 1.4) / np.tanh(1.4) * 0.9
with wave.open(sys.argv[1], 'wb') as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR)
    w.writeframes((mix * 32767).astype(np.int16).tobytes())
