# e450-collector

Reads the CII push stream of the building's main power meter (Landis+Gyr
E450) from the W6832 M-Bus dongle and exposes Prometheus metrics, including
a cumulative energy counter. Runs as a plain binary on the machine that has
the dongle.

## Installing

Download the release binary for your architecture, then install it:

```sh
curl -fsSLo /tmp/e450-collector \
  https://github.com/brutasse/e450-collector/releases/download/v0.1.0/e450-collector_0.1.0_linux_amd64
install -m 0755 /tmp/e450-collector /usr/local/bin/
```

Asset names follow the version tag (`e450-collector_<ver>_linux_arm64` for
arm64), so check the release page when a new version lands.

## Systemd

Write this to `/etc/systemd/system/e450-collector.service`:

```ini
[Unit]
Description=e450-collector, Prometheus metrics for the Landis+Gyr E450 meter

[Service]
ExecStart=/usr/local/bin/e450-collector
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Then:

```sh
systemctl daemon-reload
systemctl enable --now e450-collector
systemctl status e450-collector
journalctl -u e450-collector -f
curl -si localhost:9398/healthz    # expect HTTP/1.1 200 OK
```

The unit runs as root, which can read the dongle; to run it as a regular
user instead, add `User=e450` and `SupplementaryGroups=dialout` to
`[Service]` so it can open `/dev/ttyUSB0`.

## Building

```sh
go build -o e450-collector .
```

## Running

```sh
./e450-collector                        # serve on :9398, dongle on /dev/ttyUSB0
./e450-collector --listen=:9400         # different port
./e450-collector --dump=30s             # print push frames for 30 s and exit
./e450-collector --dump=30s --hex       # ... plus each frame as hex
```

Serves `/metrics` and `/healthz`. The dongle is a W6832 M-Bus pass-through;
the meter's CII port (RJ11) is wired to its two M-Bus pins, 2400 8N1,
security level 0. The captures in `testdata/` and the live tests used the
W6832-CHONG from Shenzhen Weihe Technology, a Prolific PL2303 USB-UART
bridge (`067b:23a3`). The meter pushes a frame every ~5 s.

## Metrics

| Metric | Meaning |
|---|---|
| `main_up` | 1 while a push has been seen within the last minute |
| `main_current_a{phase=L1\|L2\|L3\|L123}` | current (A), per phase and total |
| `main_voltage_v{phase=L1\|L2\|L3}` | line-to-neutral voltage (V), per phase |
| `main_power_w{phase=L1\|L2\|L3\|L123}` | estimated active power (W), per phase and total |
| `main_energy_wh_total{phase=L1\|L2\|L3\|L123}` | cumulative active energy (Wh), per phase and total |

The push carries no power value, only currents (1 mA resolution) and
voltages (1 V), so `main_power_w` is U x I per phase, without power factor.

`main_energy_wh_total` integrates the push values: each push adds the phase
power times the time since the previous push (monotonic clock), so the
resolution is the meter's ~5 s no matter how often Prometheus scrapes. It
starts at 0 on every boot and is never persisted; the drop to 0 is a regular
counter reset, so `increase()` and `irate()` queries stay continuous across
restarts. The push carries no energy or export register either; if PV is
added later and the push changes, inspect it with `--dump --hex`.
